package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// upsertContractMetaSQL 以 address 为幂等键刷新元信息。
//
// abi 用 COALESCE：重复解析时若本次没拿到 ABI，不应覆盖此前已获取的 ABI。
const upsertContractMetaSQL = `
INSERT INTO contract_meta (address, implementation, abi, abi_source, resolved_at)
VALUES ($1::bytea, $2::bytea, $3::jsonb, $4::text, now())
ON CONFLICT (address) DO UPDATE
SET implementation = EXCLUDED.implementation,
    abi            = COALESCE(EXCLUDED.abi, contract_meta.abi),
    abi_source     = EXCLUDED.abi_source,
    resolved_at    = EXCLUDED.resolved_at`

const loadContractMetaSQL = `
SELECT address, implementation, abi, abi_source, resolved_at
FROM contract_meta
WHERE address = $1`

// UpsertContractMeta 写入或刷新合约元信息。
//
// contract_meta.address 是主键，因此同一合约的代理识别结果不会产生重复行（HANDOFF.md 第 6 节约束 3）。
func (s *Store) UpsertContractMeta(ctx context.Context, meta ContractMeta) error {
	var implementation any
	if meta.Implementation != nil {
		implementation = meta.Implementation.Bytes()
	}

	var abiJSON any
	if len(meta.ABI) > 0 {
		abiJSON = string(meta.ABI)
	}

	if _, err := s.pool.Exec(ctx, upsertContractMetaSQL, meta.Address.Bytes(), implementation, abiJSON, meta.ABISource); err != nil {
		return fmt.Errorf("store: upsert contract meta %s: %w", meta.Address.Hex(), err)
	}

	return nil
}

// LoadContractMeta 读取合约元信息，第二个返回值为是否存在。
func (s *Store) LoadContractMeta(ctx context.Context, addr common.Address) (ContractMeta, bool, error) {
	var (
		meta           ContractMeta
		addressBytes   []byte
		implementation []byte
		abiJSON        []byte
	)

	err := s.pool.QueryRow(ctx, loadContractMetaSQL, addr.Bytes()).Scan(
		&addressBytes,
		&implementation,
		&abiJSON,
		&meta.ABISource,
		&meta.ResolvedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ContractMeta{}, false, nil
		}
		return ContractMeta{}, false, fmt.Errorf("store: load contract meta %s: %w", addr.Hex(), err)
	}

	meta.Address = common.BytesToAddress(addressBytes)
	if len(implementation) > 0 {
		parsed := common.BytesToAddress(implementation)
		meta.Implementation = &parsed
	}
	meta.ABI = abiJSON

	return meta, true, nil
}
