package tarodb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taro/address"
	"github.com/lightninglabs/taro/asset"
	"github.com/lightninglabs/taro/tarodb/sqlite"
	"github.com/lightningnetwork/lnd/keychain"
)

type (
	// AddrQuery as a type alias for a query into the set of known
	// addresses.
	AddrQuery = sqlite.FetchAddrsParams

	// NewAddr is a type alias for the params to create a new address.
	NewAddr = sqlite.InsertAddrParams

	// Addresses is a type alias for the full address row with key locator
	// information.
	Addresses = sqlite.FetchAddrsRow

	// AddrByTaprootOutput is a type alias for returning an address by its
	// Taproot output key.
	AddrByTaprootOutput = sqlite.FetchAddrByTaprootOutputKeyRow

	// AddrManaged is a type alias for setting an address as managed.
	AddrManaged = sqlite.SetAddrManagedParams

	// UpsertAddrEvent is a type alias for creating a new address event or
	// updating an existing one.
	UpsertAddrEvent = sqlite.UpsertAddrEventParams

	// AddrEvent is a type alias for fetching an address event row.
	AddrEvent = sqlite.FetchAddrEventRow

	// AddrEventQuery is a type alias for a query into the set of known
	// address events.
	AddrEventQuery = sqlite.QueryEventIDsParams

	// AddrEventID is a type alias for fetching the ID of an address event
	// and its corresponding address.
	AddrEventID = sqlite.QueryEventIDsRow

	// Genesis is a type alias for fetching the genesis asset information.
	Genesis = sqlite.FetchGenesisByIDRow
)

// AddrBook is an interface that represents the storage backed needed to create
// the TaroAddressBook book. We need to be able to insert/fetch addresses, and
// also make internal keys since each address has an internal key and a script
// key (tho they can be the same).
type AddrBook interface {
	// UpsertAssetStore houses the methods related to inserting/updating
	// assets.
	UpsertAssetStore

	// FetchGenesisStore houses the methods related to fetching genesis
	// assets.
	FetchGenesisStore

	// FetchAddrs returns all the addresses based on the constraints of the
	// passed AddrQuery.
	FetchAddrs(ctx context.Context, arg AddrQuery) ([]Addresses, error)

	// FetchAddrByTaprootOutputKey returns a single address based on its
	// Taproot output key or a sql.ErrNoRows error if no such address
	// exists.
	FetchAddrByTaprootOutputKey(ctx context.Context,
		arg []byte) (AddrByTaprootOutput, error)

	// InsertAddr inserts a new address into the database.
	InsertAddr(ctx context.Context, arg NewAddr) (int32, error)

	// UpsertInternalKey inserts a new or updates an existing internal key
	// into the database and returns the primary key.
	UpsertInternalKey(ctx context.Context, arg InternalKey) (int32, error)

	// UpsertScriptKey inserts a new script key on disk into the DB.
	UpsertScriptKey(context.Context, NewScriptKey) (int32, error)

	// SetAddrManaged sets an address as being managed by the internal
	// wallet.
	SetAddrManaged(ctx context.Context, arg AddrManaged) error

	// UpsertManagedUTXO inserts a new or updates an existing managed UTXO
	// to disk and returns the primary key.
	UpsertManagedUTXO(ctx context.Context, arg RawManagedUTXO) (int32,
		error)

	// UpsertChainTx inserts a new or updates an existing chain tx into the
	// DB.
	UpsertChainTx(ctx context.Context, arg ChainTx) (int32, error)

	// UpsertAddrEvent inserts a new or updates an existing address event
	// and returns the primary key.
	UpsertAddrEvent(ctx context.Context, arg UpsertAddrEvent) (int32, error)

	// FetchAddrEvent returns a single address event based on its primary
	// key.
	FetchAddrEvent(ctx context.Context, id int32) (AddrEvent, error)

	// QueryEventIDs returns a list of event IDs and their corresponding
	// address IDs that match the given query parameters.
	QueryEventIDs(ctx context.Context, query AddrEventQuery) ([]AddrEventID,
		error)

	// FetchAssetProof fetches the asset proof for a given asset identified
	// by its script key.
	FetchAssetProof(ctx context.Context, scriptKey []byte) (AssetProofI,
		error)
}

// AddrBookTxOptions defines the set of db txn options the AddrBook
// understands.
type AddrBookTxOptions struct {
	// readOnly governs if a read only transaction is needed or not.
	readOnly bool
}

// ReadOnly returns true if the transaction should be read only.
//
// NOTE: This implements the TxOptions
func (a *AddrBookTxOptions) ReadOnly() bool {
	return a.readOnly
}

// NewAddrBookReadTx creates a new read transaction option set.
func NewAddrBookReadTx() AssetStoreTxOptions {
	return AssetStoreTxOptions{
		readOnly: true,
	}
}

// BatchedAddrBook is a version of the AddrBook that's capable of batched
// database operations.
type BatchedAddrBook interface {
	AddrBook

	BatchedTx[AddrBook, TxOptions]
}

// TaroAddressBook represents a storage backend for all the Taro addresses a
// daemon has created.
type TaroAddressBook struct {
	db     BatchedAddrBook
	params *address.ChainParams
}

// NewTaroAddressBook creates a new TaroAddressBook instance given a open
// BatchedAddrBook storage backend.
func NewTaroAddressBook(db BatchedAddrBook,
	params *address.ChainParams) *TaroAddressBook {

	return &TaroAddressBook{
		db:     db,
		params: params,
	}
}

// insertInternalKey inserts a new internal key into the DB and returns the
// primary key of the internal key.
func insertInternalKey(ctx context.Context, a AddrBook,
	desc keychain.KeyDescriptor) (int32, error) {

	return a.UpsertInternalKey(ctx, InternalKey{
		RawKey:    desc.PubKey.SerializeCompressed(),
		KeyFamily: int32(desc.Family),
		KeyIndex:  int32(desc.Index),
	})
}

// InsertAddrs inserts a new address into the database.
func (t *TaroAddressBook) InsertAddrs(ctx context.Context,
	addrs ...address.AddrWithKeyInfo) error {

	var writeTxOpts AddrBookTxOptions
	return t.db.ExecTx(ctx, &writeTxOpts, func(db AddrBook) error {
		// For each of the addresses listed, we'll insert the two new
		// internal keys, then use those returned primary key IDs to
		// returned to insert the address itself.
		for _, addr := range addrs {
			// Make sure we have the genesis point and genesis asset
			// stored already.
			genesisPointID, err := upsertGenesisPoint(
				ctx, db, addr.FirstPrevOut,
			)
			if err != nil {
				return fmt.Errorf("unable to insert genesis "+
					"point: %w", err)
			}
			genAssetID, err := upsertGenesis(
				ctx, db, genesisPointID, addr.Genesis,
			)
			if err != nil {
				return fmt.Errorf("unable to insert genesis: "+
					"%w", err)
			}

			rawScriptKeyID, err := insertInternalKey(
				ctx, db, addr.ScriptKeyTweak.RawKey,
			)
			if err != nil {
				return fmt.Errorf("unable to insert internal "+
					"script key: %w", err)
			}
			scriptKeyID, err := db.UpsertScriptKey(ctx, NewScriptKey{
				InternalKeyID:    rawScriptKeyID,
				TweakedScriptKey: addr.ScriptKey.SerializeCompressed(),
				Tweak:            addr.ScriptKeyTweak.Tweak,
			})
			if err != nil {
				return fmt.Errorf("unable to insert script "+
					"key: %w", err)
			}

			taprootKeyID, err := insertInternalKey(
				ctx, db, addr.InternalKeyDesc,
			)
			if err != nil {
				return fmt.Errorf("unable to insert internal "+
					"taproot key: %w", err)
			}

			var famKeyBytes []byte
			if addr.FamilyKey != nil {
				famKeyBytes = addr.FamilyKey.SerializeCompressed()
			}
			_, err = db.InsertAddr(ctx, NewAddr{
				Version:        int16(addr.Version),
				GenesisAssetID: genAssetID,
				FamKey:         famKeyBytes,
				ScriptKeyID:    scriptKeyID,
				TaprootKeyID:   taprootKeyID,
				TaprootOutputKey: schnorr.SerializePubKey(
					&addr.TaprootOutputKey,
				),
				Amount:       int64(addr.Amount),
				AssetType:    int16(addr.Type),
				CreationTime: addr.CreationTime,
			})
			if err != nil {
				return fmt.Errorf("unable to insert addr: %w",
					err)
			}
		}

		return nil
	})
}

// QueryAddrs attempts to query for the set of addresses on disk given the
// passed set of query params.
func (t *TaroAddressBook) QueryAddrs(ctx context.Context,
	params address.QueryParams) ([]address.AddrWithKeyInfo, error) {

	var addrs []address.AddrWithKeyInfo

	// If the created before time is zero, then we'll use a very large date
	// to ensure that we don't restrict based on this field.
	if params.CreatedBefore.IsZero() {
		params.CreatedBefore = time.Unix(int64(math.MaxInt64), 0)
	}

	// Similarly, for sqlite using LIMIT with a value of -1 means no rows
	// should be limited.
	//
	// TODO(roasbeef): needs to be more portable
	limit := int32(-1)
	if params.Limit != 0 {
		limit = params.Limit
	}

	readOpts := NewAddrBookReadTx()
	err := t.db.ExecTx(ctx, &readOpts, func(db AddrBook) error {
		// First, fetch the set of addresses based on the set of query
		// parameters.
		dbAddrs, err := db.FetchAddrs(ctx, AddrQuery{
			CreatedAfter:  params.CreatedAfter,
			CreatedBefore: params.CreatedBefore,
			NumOffset:     int32(params.Offset),
			NumLimit:      limit,
			UnmanagedOnly: params.UnmanagedOnly,
		})
		if err != nil {
			return err
		}

		// Next, we'll need to map each of the addresses into an
		// AddrWithKeyInfo struct that can be used in a general
		// context.
		for _, addr := range dbAddrs {
			assetGenesis, err := fetchGenesis(
				ctx, db, addr.GenesisAssetID,
			)
			if err != nil {
				return fmt.Errorf("error fetching genesis: %w",
					err)
			}

			var famKey *btcec.PublicKey
			if addr.FamKey != nil {
				famKey, err = btcec.ParsePubKey(addr.FamKey)
				if err != nil {
					return fmt.Errorf("unable to decode "+
						"fam key: %w", err)
				}
			}

			rawScriptKey, err := btcec.ParsePubKey(
				addr.RawScriptKey,
			)
			if err != nil {
				return fmt.Errorf("unable to decode "+
					"script key: %w", err)
			}
			rawScriptKeyDesc := keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamily(
						addr.ScriptKeyFamily,
					),
					Index: uint32(addr.ScriptKeyIndex),
				},
				PubKey: rawScriptKey,
			}

			internalKey, err := btcec.ParsePubKey(addr.RawTaprootKey)
			if err != nil {
				return fmt.Errorf("unable to decode "+
					"taproot key: %w", err)
			}
			internalKeyDesc := keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamily(
						addr.TaprootKeyFamily,
					),
					Index: uint32(addr.TaprootKeyIndex),
				},
				PubKey: internalKey,
			}

			scriptKey, err := btcec.ParsePubKey(addr.TweakedScriptKey)
			if err != nil {
				return err
			}

			taprootOutputKey, err := schnorr.ParsePubKey(
				addr.TaprootOutputKey,
			)
			if err != nil {
				return fmt.Errorf("unable to parse taproot "+
					"output key: %w", err)
			}

			addrs = append(addrs, address.AddrWithKeyInfo{
				Taro: &address.Taro{
					Version:     asset.Version(addr.Version),
					Genesis:     assetGenesis,
					FamilyKey:   famKey,
					ScriptKey:   *scriptKey,
					InternalKey: *internalKey,
					Amount:      uint64(addr.Amount),
					ChainParams: t.params,
				},
				ScriptKeyTweak: asset.TweakedScriptKey{
					RawKey: rawScriptKeyDesc,
					Tweak:  addr.ScriptKeyTweak,
				},
				InternalKeyDesc:  internalKeyDesc,
				TaprootOutputKey: *taprootOutputKey,
				CreationTime:     addr.CreationTime,
				ManagedAfter:     addr.ManagedFrom.Time,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return addrs, nil
}

// AddrByTaprootOutput returns a single address based on its Taproot output
// key or a sql.ErrNoRows error if no such address exists.
func (t *TaroAddressBook) AddrByTaprootOutput(ctx context.Context,
	key *btcec.PublicKey) (*address.AddrWithKeyInfo, error) {

	var (
		addr     *address.AddrWithKeyInfo
		readOpts = NewAddrBookReadTx()
	)
	err := t.db.ExecTx(ctx, &readOpts, func(db AddrBook) error {
		var err error
		addr, err = fetchAddr(ctx, db, t.params, key)
		return err
	})
	if err != nil {
		return nil, err
	}

	return addr, nil
}

// fetchAddr fetches a single address identified by its taproot output key from
// the database and populates all its fields.
func fetchAddr(ctx context.Context, db AddrBook, params *address.ChainParams,
	taprootOutputKey *btcec.PublicKey) (*address.AddrWithKeyInfo, error) {

	dbAddr, err := db.FetchAddrByTaprootOutputKey(
		ctx, schnorr.SerializePubKey(taprootOutputKey),
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, address.ErrNoAddr

	case err != nil:
		return nil, err
	}

	genesis, err := fetchGenesis(ctx, db, dbAddr.GenesisAssetID)
	if err != nil {
		return nil, fmt.Errorf("error fetching genesis: %w", err)
	}

	var famKey *btcec.PublicKey
	if dbAddr.FamKey != nil {
		famKey, err = btcec.ParsePubKey(dbAddr.FamKey)
		if err != nil {
			return nil, fmt.Errorf("unable to decode fam key: %w",
				err)
		}
	}

	rawScriptKey, err := btcec.ParsePubKey(dbAddr.RawScriptKey)
	if err != nil {
		return nil, fmt.Errorf("unable to decode script key: %w", err)
	}
	scriptKeyDesc := keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(
				dbAddr.ScriptKeyFamily,
			),
			Index: uint32(dbAddr.ScriptKeyIndex),
		},
		PubKey: rawScriptKey,
	}

	scriptKey, err := btcec.ParsePubKey(dbAddr.TweakedScriptKey)
	if err != nil {
		return nil, fmt.Errorf("unable to decode script key: %w", err)
	}

	internalKey, err := btcec.ParsePubKey(dbAddr.RawTaprootKey)
	if err != nil {
		return nil, fmt.Errorf("unable to decode taproot key: %w", err)
	}
	internalKeyDesc := keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(
				dbAddr.TaprootKeyFamily,
			),
			Index: uint32(dbAddr.TaprootKeyIndex),
		},
		PubKey: internalKey,
	}

	return &address.AddrWithKeyInfo{
		Taro: &address.Taro{
			Version:     asset.Version(dbAddr.Version),
			Genesis:     genesis,
			FamilyKey:   famKey,
			ScriptKey:   *scriptKey,
			InternalKey: *internalKey,
			Amount:      uint64(dbAddr.Amount),
			ChainParams: params,
		},
		ScriptKeyTweak: asset.TweakedScriptKey{
			RawKey: scriptKeyDesc,
			Tweak:  dbAddr.ScriptKeyTweak,
		},
		InternalKeyDesc:  internalKeyDesc,
		TaprootOutputKey: *taprootOutputKey,
		CreationTime:     dbAddr.CreationTime,
	}, nil
}

// SetAddrManaged sets an address as being managed by the internal
// wallet.
func (t *TaroAddressBook) SetAddrManaged(ctx context.Context,
	addr *address.AddrWithKeyInfo, managedFrom time.Time) error {

	var writeTxOpts AddrBookTxOptions
	return t.db.ExecTx(ctx, &writeTxOpts, func(db AddrBook) error {
		return db.SetAddrManaged(ctx, AddrManaged{
			ManagedFrom: sql.NullTime{
				Time:  managedFrom,
				Valid: true,
			},
			TaprootOutputKey: schnorr.SerializePubKey(
				&addr.TaprootOutputKey,
			),
		})
	})
}

// GetOrCreateEvent creates a new address event for the given status, address
// and transaction. If an event for that address and transaction already exists,
// then the status and transaction information is updated instead.
func (t *TaroAddressBook) GetOrCreateEvent(ctx context.Context,
	status address.Status, addr *address.AddrWithKeyInfo,
	walletTx *lndclient.Transaction, outputIdx uint32,
	tapscriptSibling *chainhash.Hash) (*address.Event, error) {

	var (
		writeTxOpts  AddrBookTxOptions
		event        *address.Event
		txHash       = walletTx.Tx.TxHash()
		txBuf        bytes.Buffer
		siblingBytes []byte
	)
	if err := walletTx.Tx.Serialize(&txBuf); err != nil {
		return nil, fmt.Errorf("error serializing tx: %w", err)
	}
	outpoint := wire.OutPoint{
		Hash:  txHash,
		Index: outputIdx,
	}
	outpointBytes, err := encodeOutpoint(outpoint)
	if err != nil {
		return nil, fmt.Errorf("error encoding outpoint: %w", err)
	}
	outputDetails := walletTx.OutputDetails[outputIdx]

	if tapscriptSibling != nil {
		siblingBytes = tapscriptSibling[:]
	}

	dbErr := t.db.ExecTx(ctx, &writeTxOpts, func(db AddrBook) error {
		// The first step is to make sure we already track the on-chain
		// transaction in our DB.
		txUpsert := ChainTx{
			Txid:  txHash[:],
			RawTx: txBuf.Bytes(),
		}
		if walletTx.Confirmations > 0 {
			txUpsert.BlockHeight.Valid = true
			txUpsert.BlockHeight.Int32 = walletTx.BlockHeight

			// We're missing the transaction index within the block,
			// we need to update that from the proof. Fortunately we
			// only update fields that aren't nil in the upsert.
			blockHash, err := chainhash.NewHashFromStr(
				walletTx.BlockHash,
			)
			if err != nil {
				return fmt.Errorf("error parsing block hash: "+
					"%w", err)
			}
			txUpsert.BlockHash = blockHash[:]
		}
		chainTxID, err := db.UpsertChainTx(ctx, txUpsert)
		if err != nil {
			return fmt.Errorf("error upserting chain TX: %w", err)
		}

		commitment, err := addr.TaroCommitment()
		if err != nil {
			return fmt.Errorf("error deriving commitment: %w", err)
		}
		taroRoot := commitment.TapscriptRoot(tapscriptSibling)

		utxoUpsert := RawManagedUTXO{
			RawKey:           addr.InternalKey.SerializeCompressed(),
			Outpoint:         outpointBytes,
			AmtSats:          outputDetails.Amount,
			TapscriptSibling: siblingBytes,
			TaroRoot:         taroRoot[:],
			TxnID:            chainTxID,
		}
		managedUtxoID, err := db.UpsertManagedUTXO(ctx, utxoUpsert)
		if err != nil {
			return fmt.Errorf("error upserting utxo: %w", err)
		}

		eventID, err := db.UpsertAddrEvent(ctx, UpsertAddrEvent{
			TaprootOutputKey: schnorr.SerializePubKey(
				&addr.TaprootOutputKey,
			),
			CreationTime:        time.Now(),
			Status:              int16(status),
			Txid:                txHash[:],
			ChainTxnOutputIndex: int32(outputIdx),
			ManagedUtxoID:       managedUtxoID,
		})
		if err != nil {
			return fmt.Errorf("error fetching existing events: %w",
				err)
		}

		event, err = fetchEvent(ctx, db, eventID, addr)
		return err
	})
	if dbErr != nil {
		return nil, dbErr
	}

	return event, nil
}

// QueryAddrEvents returns a list of event that match the given query
// parameters.
func (t *TaroAddressBook) QueryAddrEvents(
	ctx context.Context, params address.EventQueryParams) ([]*address.Event,
	error) {

	sqlQuery := AddrEventQuery{
		StatusFrom: int16(address.StatusTransactionDetected),
		StatusTo:   int16(address.StatusCompleted),
	}
	if len(params.AddrTaprootOutputKey) > 0 {
		sqlQuery.AddrTaprootKey = params.AddrTaprootOutputKey
	}
	if params.StatusFrom != nil {
		sqlQuery.StatusFrom = int16(*params.StatusFrom)
	}
	if params.StatusTo != nil {
		sqlQuery.StatusTo = int16(*params.StatusTo)
	}

	var (
		readTxOpts = NewAssetStoreReadTx()
		events     []*address.Event
	)
	err := t.db.ExecTx(ctx, &readTxOpts, func(db AddrBook) error {
		dbIDs, err := db.QueryEventIDs(ctx, sqlQuery)
		if err != nil {
			return fmt.Errorf("error fetching event IDs: %w", err)
		}

		events = make([]*address.Event, len(dbIDs))
		for idx, ids := range dbIDs {
			taprootOutputKey, err := schnorr.ParsePubKey(
				ids.TaprootOutputKey,
			)
			if err != nil {
				return fmt.Errorf("error parsing taproot "+
					"output key: %w", err)
			}

			addr, err := fetchAddr(
				ctx, db, t.params, taprootOutputKey,
			)
			if err != nil {
				return fmt.Errorf("error fetching address: %w",
					err)
			}

			event, err := fetchEvent(ctx, db, ids.EventID, addr)
			if err != nil {
				return fmt.Errorf("error fetching address "+
					"event: %w", err)
			}

			events[idx] = event
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return events, nil
}

// fetchEvent fetches a single address event identified by its primary ID and
// address.
func fetchEvent(ctx context.Context, db AddrBook, eventID int32,
	addr *address.AddrWithKeyInfo) (*address.Event, error) {

	dbEvent, err := db.FetchAddrEvent(ctx, eventID)
	if err != nil {
		return nil, fmt.Errorf("error fetching addr event: %w", err)
	}

	internalKey, err := btcec.ParsePubKey(dbEvent.InternalKey)
	if err != nil {
		return nil, fmt.Errorf("error parsing internal key: %w", err)
	}

	hash, err := chainhash.NewHash(dbEvent.Txid)
	if err != nil {
		return nil, fmt.Errorf("error parsing txid: %w", err)
	}
	op := wire.OutPoint{
		Hash:  *hash,
		Index: uint32(dbEvent.OutputIndex),
	}

	return &address.Event{
		ID:                 eventID,
		CreationTime:       dbEvent.CreationTime,
		Addr:               addr,
		Status:             address.Status(dbEvent.Status),
		Outpoint:           op,
		Amt:                btcutil.Amount(dbEvent.AmtSats.Int64),
		InternalKey:        internalKey,
		TapscriptSibling:   dbEvent.TapscriptSibling,
		ConfirmationHeight: uint32(dbEvent.ConfirmationHeight.Int32),
		HasProof:           dbEvent.AssetProofID.Valid,
	}, nil
}

// CompleteEvent updates an address event as being complete and links it with
// the proof and asset that was imported/created for it.
func (t *TaroAddressBook) CompleteEvent(ctx context.Context,
	event *address.Event, status address.Status,
	anchorPoint wire.OutPoint) error {

	scriptKeyBytes := event.Addr.ScriptKey.SerializeCompressed()

	var writeTxOpts AddrBookTxOptions
	return t.db.ExecTx(ctx, &writeTxOpts, func(db AddrBook) error {
		proofData, err := db.FetchAssetProof(ctx, scriptKeyBytes)
		if err != nil {
			return fmt.Errorf("error fetching asset proof: %w", err)
		}

		_, err = db.UpsertAddrEvent(ctx, UpsertAddrEvent{
			TaprootOutputKey: schnorr.SerializePubKey(
				&event.Addr.TaprootOutputKey,
			),
			Status:              int16(status),
			Txid:                anchorPoint.Hash[:],
			ChainTxnOutputIndex: int32(anchorPoint.Index),
			AssetProofID:        sqlInt32(proofData.ProofID),
			AssetID:             sqlInt32(proofData.AssetID),
		})
		return err
	})
}

// A set of compile-time assertions to ensure that TaroAddressBook meets the
// address.Storage and address.EventStorage interface.
var _ address.Storage = (*TaroAddressBook)(nil)
var _ address.EventStorage = (*TaroAddressBook)(nil)
