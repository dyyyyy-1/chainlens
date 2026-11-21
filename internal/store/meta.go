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
// abi 与 decimals 都用 COALESCE：重复解析时若本次没拿到值，不应覆盖此前已获取的值。
// 这一条对 decimals 尤其重要——RPC 抖动一次就把已知精度抹成 NULL，前端会退回猜测。
const upsertContractMetaSQL = `
INSERT INTO contract_meta (address, implementation, abi, abi_source, decimals, resolved_at)
VALUES ($1::bytea, $2::bytea, $3::jsonb, $4::text, $5::smallint, now())
ON CONFLICT (address) DO UPDATE
SET implementation = EXCLUDED.implementation,
    abi            = COALESCE(EXCLUDED.abi, contract_meta.abi),
    abi_source     = EXCLUDED.abi_source,
    decimals       = COALESCE(EXCLUDED.decimals, contract_meta.decimals),
    resolved_at    = EXCLUDED.resolved_at`

const loadContractMetaSQL = `
SELECT address, implementation, abi, abi_source, decimals, resolved_at
FROM contract_meta
WHERE address = $1`

// decimalsByContractsSQL 只取 decimals，不把 ABI 一起拉回来。
//
// 一个合约的 ABI 可达数十 KB；画像接口每次都要为若干合约取精度，带上 ABI 是纯浪费。
const decimalsByContractsSQL = `
SELECT address, decimals
FROM contract_meta
WHERE address = ANY($1::bytea[]) AND decimals IS NOT NULL`

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

	var decimals any
	if meta.Decimals != nil {
		// 用 int16 而不是 uint8：pgx 对 uint8 的映射不保证走 smallint。
		decimals = int16(*meta.Decimals)
	}

	if _, err := s.pool.Exec(ctx, upsertContractMetaSQL,
		meta.Address.Bytes(), implementation, abiJSON, meta.ABISource, decimals); err != nil {
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
		decimals       *int16
	)

	err := s.pool.QueryRow(ctx, loadContractMetaSQL, addr.Bytes()).Scan(
		&addressBytes,
		&implementation,
		&abiJSON,
		&meta.ABISource,
		&decimals,
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
	if decimals != nil {
		value := uint8(*decimals)
		meta.Decimals = &value
	}

	return meta, true, nil
}

// DecimalsByContracts 批量取合约精度，只返回已知精度的合约。
//
// 未知精度的合约不会出现在结果里，调用方据此返回 null，而不是拿 0 或默认值顶替。
func (s *Store) DecimalsByContracts(ctx context.Context, addresses []common.Address) (map[common.Address]uint8, error) {
	if len(addresses) == 0 {
		return map[common.Address]uint8{}, nil
	}

	raw := make([][]byte, len(addresses))
	for i, address := range addresses {
		raw[i] = address.Bytes()
	}

	rows, err := s.pool.Query(ctx, decimalsByContractsSQL, raw)
	if err != nil {
		return nil, fmt.Errorf("store: load decimals for %d contracts: %w", len(addresses), err)
	}
	defer rows.Close()

	decimals := make(map[common.Address]uint8, len(addresses))
	for rows.Next() {
		var (
			addressBytes []byte
			value        int16
		)
		if err := rows.Scan(&addressBytes, &value); err != nil {
			return nil, fmt.Errorf("store: scan decimals: %w", err)
		}
		decimals[common.BytesToAddress(addressBytes)] = uint8(value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load decimals for %d contracts: %w", len(addresses), err)
	}

	return decimals, nil
}
