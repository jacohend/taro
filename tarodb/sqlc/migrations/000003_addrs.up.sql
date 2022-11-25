-- addrs stores all the created addresses of the daemon. All addresses contain
-- a creation time and all the information needed to reconstruct the taproot
-- output on chain we'll use to send/recv to/from this address.
CREATE TABLE IF NOT EXISTS addrs (
    id INTEGER PRIMARY KEY,

    -- version is the Taro script version this address support.
    version SMALLINT NOT NULL,

    -- genesis_asset_id points to the asset genesis of the asset we want to
    -- send/recv.
    genesis_asset_id INTEGER NOT NULL REFERENCES genesis_assets(gen_asset_id),

    -- fam_key is the raw blob of the family key. For assets w/o a family key,
    -- this field will be NULL.
    fam_key BLOB,

    -- script_key_id points to the internal key that we created to serve as the
    -- script key to be able to receive this asset.
    script_key_id INTEGER NOT NULL REFERENCES script_keys(script_key_id),

    -- taproot_key_id points to the internal key that we'll use to serve as the
    -- taproot internal key to receive this asset.
    taproot_key_id INTEGER NOT NULL REFERENCES internal_keys(key_id),

    -- taproot_output_key is the tweaked taproot output key that assets must
    -- be sent to on chain to be received, represented as a 32-byte x-only
    -- public key.
    taproot_output_key BLOB NOT NULL UNIQUE CHECK(length(taproot_output_key) = 32),

    -- amount is the amount of asset we want to receive.
    amount BIGINT NOT NULL,  

    -- asset_type is the type of asset we want to receive. 
    asset_type SMALLINT NOT NULL,

    -- creation_time is the creation time of this asset.
    creation_time TIMESTAMP NOT NULL,

    -- managed_from is the timestamp at which the address started to be managed
    -- by the internal wallet.
    managed_from TIMESTAMP
);

-- We'll create some indexes over the asset ID, family key, and also creation
-- time to speed up common queries.
CREATE INDEX IF NOT EXISTS addr_asset_genesis_ids ON addrs (genesis_asset_id);
CREATE INDEX IF NOT EXISTS addr_fam_keys ON addrs (fam_key);
CREATE INDEX IF NOT EXISTS addr_creation_time ON addrs (creation_time);
CREATE INDEX IF NOT EXISTS addr_managed_from ON addrs (managed_from);
