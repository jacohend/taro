package tarodb

import (
	"context"
	"database/sql"
	"math/rand"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taro/address"
	"github.com/lightninglabs/taro/internal/test"
	"github.com/lightninglabs/taro/tarodb/sqlc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
)

var (
	chainParams = &address.RegressionNetTaro
)

// newAddrBook makes a new instance of the TaroAddressBook book.
func newAddrBook(t *testing.T) (*TaroAddressBook, sqlc.Querier) {
	db := NewTestDB(t)

	txCreator := func(tx *sql.Tx) AddrBook {
		return db.WithTx(tx)
	}

	addrTx := NewTransactionExecutor[AddrBook](db, txCreator)
	return NewTaroAddressBook(addrTx, chainParams), db
}

func confirmTx(tx *lndclient.Transaction) {
	blockHash := test.RandHash()
	tx.Confirmations = rand.Int31n(50) + 1
	tx.BlockHash = blockHash.String()
	tx.BlockHeight = rand.Int31n(700_000)
}

func randWalletTx() *lndclient.Transaction {
	tx := &lndclient.Transaction{
		Tx:        wire.NewMsgTx(2),
		Timestamp: time.Now(),
	}
	numInputs := rand.Intn(10) + 1
	numOutputs := rand.Intn(5) + 1

	for idx := 0; idx < numInputs; idx++ {
		in := &wire.TxIn{}
		_, _ = rand.Read(in.PreviousOutPoint.Hash[:])
		in.PreviousOutPoint.Index = rand.Uint32()
		tx.Tx.AddTxIn(in)
		tx.PreviousOutpoints = append(
			tx.PreviousOutpoints, &lnrpc.PreviousOutPoint{
				Outpoint:    in.PreviousOutPoint.String(),
				IsOurOutput: rand.Int31()%2 == 0,
			},
		)
	}
	for idx := 0; idx < numOutputs; idx++ {
		out := &wire.TxOut{
			Value: rand.Int63n(5000000),
		}
		out.PkScript = make([]byte, 34)
		_, _ = rand.Read(out.PkScript)
		tx.Tx.AddTxOut(out)
		tx.OutputDetails = append(
			tx.OutputDetails, &lnrpc.OutputDetail{
				Amount:       out.Value,
				IsOurAddress: rand.Int31()%2 == 0,
			},
		)
	}

	return tx
}

// assertEqualAddrs makes sure the given actual addresses match the expected
// ones.
func assertEqualAddrs(t *testing.T, expected, actual []address.AddrWithKeyInfo) {
	require.Len(t, actual, len(expected))
	for idx := range actual {
		assertEqualAddr(t, expected[idx], actual[idx])
	}
}

// assertEqualAddr makes sure the given actual address matches the expected
// one
func assertEqualAddr(t *testing.T, expected, actual address.AddrWithKeyInfo) {
	// Time values cannot be compared based on their struct contents
	// since the same time can be represented in different ways.
	// We compare the addresses without the timestamps and then
	// compare the unix timestamps separately.
	actualTime := actual.CreationTime
	expectedTime := expected.CreationTime

	actual.CreationTime = time.Time{}
	expected.CreationTime = time.Time{}

	require.Equal(t, expected, actual)
	require.Equal(t, expectedTime.Unix(), actualTime.Unix())
}

// assertEqualAddrEvents makes sure the given actual address events match the
// expected ones.
func assertEqualAddrEvents(t *testing.T, expected, actual []*address.Event) {
	require.Len(t, actual, len(expected))
	for idx := range actual {
		assertEqualAddrEvent(t, *expected[idx], *actual[idx])
	}
}

// assertEqualAddrEvent makes sure the given actual address event matches the
// expected one.
func assertEqualAddrEvent(t *testing.T, expected, actual address.Event) {
	assertEqualAddr(t, *expected.Addr, *actual.Addr)
	actual.Addr = nil
	expected.Addr = nil

	// Time values cannot be compared based on their struct contents
	// since the same time can be represented in different ways.
	// We compare the addresses without the timestamps and then
	// compare the unix timestamps separately.
	actualTime := actual.CreationTime
	expectedTime := expected.CreationTime

	actual.CreationTime = time.Time{}
	expected.CreationTime = time.Time{}

	require.Equal(t, expected, actual)
	require.Equal(t, expectedTime.Unix(), actualTime.Unix())
}

// TestAddressInsertion tests that we're always able to retrieve an address we
// inserted into the DB.
func TestAddressInsertion(t *testing.T) {
	t.Parallel()

	// First, make a new addr book instance we'll use in the test below.
	addrBook, _ := newAddrBook(t)

	// Make a series of new addrs, then insert them into the DB.
	const numAddrs = 5
	addrs := make([]address.AddrWithKeyInfo, numAddrs)
	for i := 0; i < numAddrs; i++ {
		addrs[i] = *address.RandAddr(t, chainParams)
	}
	ctx := context.Background()
	require.NoError(t, addrBook.InsertAddrs(ctx, addrs...))

	// Now we should be able to fetch the complete set of addresses with
	// the query method without specifying any special params.
	dbAddrs, err := addrBook.QueryAddrs(ctx, address.QueryParams{})
	require.NoError(t, err)

	// The returned addresses should match up exactly.
	require.Len(t, dbAddrs, numAddrs)
	assertEqualAddrs(t, addrs, dbAddrs)

	// Make sure that we can fetch each address by its Taproot output key as
	// well.
	for _, addr := range addrs {
		dbAddr, err := addrBook.AddrByTaprootOutput(
			ctx, &addr.TaprootOutputKey,
		)
		require.NoError(t, err)
		assertEqualAddr(t, addr, *dbAddr)
	}

	// All addresses should be unmanaged at this point.
	dbAddrs, err = addrBook.QueryAddrs(ctx, address.QueryParams{
		UnmanagedOnly: true,
	})
	require.NoError(t, err)
	require.Len(t, dbAddrs, numAddrs)
	assertEqualAddrs(t, addrs, dbAddrs)

	// Declare the first two addresses as managed.
	managedFrom := time.Now()
	err = addrBook.SetAddrManaged(ctx, &dbAddrs[0], managedFrom)
	require.NoError(t, err)
	err = addrBook.SetAddrManaged(ctx, &dbAddrs[1], managedFrom)
	require.NoError(t, err)

	// Make sure the unmanaged are now distinct from the rest.
	dbAddrs, err = addrBook.QueryAddrs(ctx, address.QueryParams{
		UnmanagedOnly: true,
	})
	require.NoError(t, err)
	require.Len(t, dbAddrs, 3)

	// The ORDER BY clause should make sure the unmanaged addresses are
	// actually the last three.
	assertEqualAddr(t, addrs[2], dbAddrs[0])
	assertEqualAddr(t, addrs[3], dbAddrs[1])
	assertEqualAddr(t, addrs[4], dbAddrs[2])

	// But a query with no filter still returns all addresses.
	dbAddrs, err = addrBook.QueryAddrs(ctx, address.QueryParams{})
	require.NoError(t, err)
	require.Len(t, dbAddrs, numAddrs)

	require.Equal(t, managedFrom.Unix(), dbAddrs[0].ManagedAfter.Unix())
	require.Equal(t, managedFrom.Unix(), dbAddrs[1].ManagedAfter.Unix())
}

// TestAddressQuery tests that we're able to properly retrieve rows based on
// various combinations of the query parameters.
func TestAddressQuery(t *testing.T) {
	t.Parallel()

	// First, make a new addr book instance we'll use in the test below.
	addrBook, _ := newAddrBook(t)

	// Make a series of new addrs, then insert them into the DB.
	const numAddrs = 5
	addrs := make([]address.AddrWithKeyInfo, numAddrs)
	for i := 0; i < numAddrs; i++ {
		addrs[i] = *address.RandAddr(t, chainParams)
	}
	ctx := context.Background()
	require.NoError(t, addrBook.InsertAddrs(ctx, addrs...))

	tests := []struct {
		name string

		createdAfter  time.Time
		createdBefore time.Time
		limit         int32
		offset        int32
		unmanagedOnly bool

		numAddrs   int
		firstIndex int
	}{
		// No params, all rows should be returned.
		{
			name: "no params",

			numAddrs: numAddrs,
		},

		// Limit value should be respected.
		{
			name: "limit",

			limit:    2,
			numAddrs: 2,
		},

		// We should be able to offset from the limit.
		{
			name: "limit+offset",

			limit:  2,
			offset: 1,

			numAddrs:   2,
			firstIndex: 1,
		},

		// Created after in the future should return no rows.
		{
			name: "created after",

			createdAfter: time.Now().Add(time.Hour * 24),
			numAddrs:     0,
		},

		// Created before in the future should return all the rows.
		{
			name: "created before",

			createdBefore: time.Now().Add(time.Hour * 24),
			numAddrs:      numAddrs,
		},

		// Created before in the past should return all the rows.
		{
			name: "created before past",

			createdBefore: time.Now().Add(-time.Hour * 24),
			numAddrs:      0,
		},

		// Unmanaged only, which is the full list.
		{
			name: "unmanaged only",

			unmanagedOnly: true,
			numAddrs:      numAddrs,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dbAddrs, err := addrBook.QueryAddrs(
				ctx, address.QueryParams{
					CreatedAfter:  test.createdAfter,
					CreatedBefore: test.createdBefore,
					Offset:        test.offset,
					Limit:         test.limit,
					UnmanagedOnly: test.unmanagedOnly,
				},
			)
			require.NoError(t, err)
			require.Len(t, dbAddrs, test.numAddrs)
		})
	}
}

// TestAddrEventStatusDBEnum makes sure we cannot insert an event with an
// invalid status into the database.
func TestAddrEventStatusDBEnum(t *testing.T) {
	t.Parallel()

	// First, make a new addr book instance we'll use in the test below.
	addrBook, _ := newAddrBook(t)

	ctx := context.Background()

	// Make sure an event with an invalid status cannot be created. This
	// should be protected by a CHECK constraint on the column. If this
	// fails, you need to update that constraint in the DB!
	addr := address.RandAddr(t, chainParams)
	err := addrBook.InsertAddrs(ctx, *addr)
	require.NoError(t, err)

	txn := randWalletTx()
	outputIndex := rand.Intn(len(txn.Tx.TxOut))

	_, err = addrBook.GetOrCreateEvent(
		ctx, address.Status(4), addr, txn, uint32(outputIndex), nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "constraint")
}

// TestAddrEventCreation tests that address events can be created and updated
// correctly.
func TestAddrEventCreation(t *testing.T) {
	t.Parallel()

	// First, make a new addr book instance we'll use in the test below.
	addrBook, _ := newAddrBook(t)

	ctx := context.Background()

	// Create 5 addresses and then events with unconfirmed transactions.
	const numAddrs = 5
	txns := make([]*lndclient.Transaction, numAddrs)
	events := make([]*address.Event, numAddrs)
	for i := 0; i < numAddrs; i++ {
		addr := address.RandAddr(t, chainParams)
		err := addrBook.InsertAddrs(ctx, *addr)
		require.NoError(t, err)

		txns[i] = randWalletTx()
		outputIndex := rand.Intn(len(txns[i].Tx.TxOut))

		var tapscriptSibling *chainhash.Hash
		if rand.Int31()%2 == 0 {
			hash := test.RandHash()
			tapscriptSibling = &hash
		}
		event, err := addrBook.GetOrCreateEvent(
			ctx, address.StatusTransactionDetected, addr, txns[i],
			uint32(outputIndex), tapscriptSibling,
		)
		require.NoError(t, err)

		events[i] = event
	}

	// All 5 events should be returned when querying pending events.
	pendingEvents, err := addrBook.QueryAddrEvents(
		ctx, address.EventQueryParams{},
	)
	require.NoError(t, err)
	assertEqualAddrEvents(t, events, pendingEvents)

	// If we try to create the same events again, we should just get the
	// exact same event back.
	for idx := range events {
		var tapscriptSibling *chainhash.Hash
		if len(events[idx].TapscriptSibling) > 0 {
			tapscriptSibling, err = chainhash.NewHash(
				events[idx].TapscriptSibling,
			)
			require.NoError(t, err)
		}

		actual, err := addrBook.GetOrCreateEvent(
			ctx, address.StatusTransactionDetected,
			events[idx].Addr, txns[idx], events[idx].Outpoint.Index,
			tapscriptSibling,
		)
		require.NoError(t, err)

		assertEqualAddrEvent(t, *events[idx], *actual)
	}

	// Now we update the status of our event, make the transaction confirmed
	// and set the tapscript sibling to nil for all of them.
	for idx := range events {
		confirmTx(txns[idx])
		events[idx].Status = address.StatusTransactionConfirmed
		events[idx].ConfirmationHeight = uint32(txns[idx].BlockHeight)

		actual, err := addrBook.GetOrCreateEvent(
			ctx, address.StatusTransactionConfirmed,
			events[idx].Addr, txns[idx], events[idx].Outpoint.Index,
			nil,
		)
		require.NoError(t, err)

		assertEqualAddrEvent(t, *events[idx], *actual)
	}
}

// TestAddressEventQuery tests that we're able to properly retrieve rows based
// on various combinations of the query parameters.
func TestAddressEventQuery(t *testing.T) {
	t.Parallel()

	// First, make a new addr book instance we'll use in the test below.
	addrBook, _ := newAddrBook(t)

	ctx := context.Background()

	// Make a series of new addrs, then insert them into the DB.
	const numAddrs = 5
	addrs := make([]address.AddrWithKeyInfo, numAddrs)
	for i := 0; i < numAddrs; i++ {
		addr := address.RandAddr(t, chainParams)
		require.NoError(t, addrBook.InsertAddrs(ctx, *addr))

		txn := randWalletTx()
		outputIndex := rand.Intn(len(txn.Tx.TxOut))

		// Make sure we use all states at least once.
		status := address.Status(i % int(address.StatusCompleted+1))
		event, err := addrBook.GetOrCreateEvent(
			ctx, status, addr, txn, uint32(outputIndex), nil,
		)
		require.NoError(t, err)
		require.EqualValues(t, i+1, event.ID)

		addrs[i] = *addr
	}

	var (
		confirmed = address.StatusTransactionConfirmed
		invalid   = address.Status(123)
	)

	tests := []struct {
		name string

		addrTaprootKey []byte
		stateFrom      *address.Status
		stateTo        *address.Status

		numAddrs int
		firstID  int
	}{
		// No params, all rows should be returned.
		{
			name: "no params",

			numAddrs: numAddrs,
		},

		// Invalid status.
		{
			name: "invalid status",

			stateFrom: &invalid,
			numAddrs:  0,
		},

		// Invalid key.
		{
			name: "invalid address taproot key",

			addrTaprootKey: []byte{99, 99},
			numAddrs:       0,
		},

		// Exactly one status.
		{
			name:      "single status",
			stateFrom: &confirmed,
			stateTo:   &confirmed,

			numAddrs: 1,
			firstID:  2,
		},

		// Empty taproot key slice.
		{
			name: "empty address taproot key",

			addrTaprootKey: []byte{},
			numAddrs:       5,
		},

		// Correct key.
		{
			name: "correct address taproot key",

			addrTaprootKey: schnorr.SerializePubKey(
				&addrs[4].TaprootOutputKey,
			),
			numAddrs: 1,
			firstID:  5,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test := test
			dbAddrs, err := addrBook.QueryAddrEvents(
				ctx, address.EventQueryParams{
					AddrTaprootOutputKey: test.addrTaprootKey,
					StatusFrom:           test.stateFrom,
					StatusTo:             test.stateTo,
				},
			)
			require.NoError(t, err)
			require.Len(t, dbAddrs, test.numAddrs)

			if test.firstID > 0 {
				require.EqualValues(
					t, dbAddrs[0].ID, test.firstID,
				)
			}
		})
	}
}
