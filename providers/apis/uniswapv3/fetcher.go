package uniswapv3

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"go.uber.org/zap"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/skip-mev/slinky/oracle/config"
	"github.com/skip-mev/slinky/oracle/types"
	uniswappool "github.com/skip-mev/slinky/providers/apis/uniswapv3/pool"
	"github.com/skip-mev/slinky/providers/base/api/metrics"
	providertypes "github.com/skip-mev/slinky/providers/types"
	mmtypes "github.com/skip-mev/slinky/x/marketmap/types"
)

var _ types.PriceAPIFetcher = (*UniswapV3PriceFetcher)(nil)

// UniswapV3PriceFetcher is the Uniswap V3 price fetcher. This fetcher is responsible for
// querying Uniswap V3 pool contracts and returning the price of a given ticker. The price is
// derived from the slot 0 data of the pool contract.
//
// To read more about how the price is calculated, see the Uniswap V3 documentation
// https://blog.uniswap.org/uniswap-v3-math-primer.
//
// We utilize the eth client's BatchCallContext to batch the calls to the ethereum network as
// this is more performant than making individual calls or the multi call contract:
// https://docs.chainstack.com/docs/http-batch-request-vs-multicall-contract#performance-comparison.
type UniswapV3PriceFetcher struct {
	logger  *zap.Logger
	metrics metrics.APIMetrics
	api     config.APIConfig

	// client is the EVM client implementation. This is used to interact with the ethereum network.
	client EVMClient
	// abi is the uniswap v3 pool abi. This is used to pack the slot0 call to the pool contract
	// and parse the result.
	abi *abi.ABI
	// payload is the packed slot0 call to the pool contract. Since the slot0 payload is the same
	// for all pools, we can reuse this payload for all pools.
	payload []byte
	// poolCache is a cache of the tickers to pool configs. This is used to avoid unmarshalling
	// the metadata for each ticker.
	poolCache map[mmtypes.Ticker]PoolConfig
}

// NewUniswapV3PriceFetcher returns a new Uniswap V3 price fetcher.
func NewUniswapV3PriceFetcher(
	logger *zap.Logger,
	metrics metrics.APIMetrics,
	api config.APIConfig,
	client EVMClient,
) (*UniswapV3PriceFetcher, error) {
	if logger == nil {
		return nil, fmt.Errorf("logger cannot be nil")
	}

	if metrics == nil {
		return nil, fmt.Errorf("metrics cannot be nil")
	}

	if api.Name != Name {
		return nil, fmt.Errorf("expected api config name %s, got %s", Name, api.Name)
	}

	if !api.Enabled {
		return nil, fmt.Errorf("api config for %s is not enabled", Name)
	}

	if err := api.ValidateBasic(); err != nil {
		return nil, fmt.Errorf("invalid api config: %w", err)
	}

	abi, err := uniswappool.UniswapMetaData.GetAbi()
	if err != nil {
		return nil, fmt.Errorf("failed to get uniswap abi: %w", err)
	}

	payload, err := abi.Pack("slot0")
	if err != nil {
		return nil, fmt.Errorf("failed to pack slot0: %w", err)
	}

	return &UniswapV3PriceFetcher{
		logger:    logger,
		metrics:   metrics,
		api:       api,
		client:    client,
		abi:       abi,
		payload:   payload,
		poolCache: make(map[mmtypes.Ticker]PoolConfig),
	}, nil
}

// Fetch returns the price of a given set of tickers. This fetch utilizes the batch call to lower
// overhead of making individual RPC calls for each ticker. The fetcher will query the Uniswap V3
// pool contract for the price of the pool. The price is derived from the slot 0 data of the pool
// contract, specifically the sqrtPriceX96 value.
func (u *UniswapV3PriceFetcher) Fetch(
	ctx context.Context,
	tickers []mmtypes.Ticker,
) types.PriceResponse {
	start := time.Now()
	defer func() {
		u.metrics.ObserveProviderResponseLatency(Name, time.Since(start))
	}()

	var (
		resolved   = make(types.ResolvedPrices)
		unResolved = make(types.UnResolvedPrices)
	)

	// Create a batch element for each ticker and pool.
	batchElems := make([]rpc.BatchElem, len(tickers))
	pools := make([]PoolConfig, len(tickers))
	for i, ticker := range tickers {
		pool, err := u.GetPool(ticker)
		if err != nil {
			u.logger.Error(
				"failed to get pool for ticker",
				zap.String("ticker", ticker.String()),
				zap.Error(err),
			)

			return types.NewPriceResponseWithErr(
				tickers,
				providertypes.NewErrorWithCode(
					fmt.Errorf("failed to get pool: %w", err),
					providertypes.ErrorFailedToDecode,
				),
			)
		}

		// Create a batch element for the ticker and pool.
		var result string
		batchElems[i] = rpc.BatchElem{
			Method: "eth_call",
			Args: []interface{}{
				map[string]interface{}{
					"to":   common.HexToAddress(pool.Address),
					"data": hexutil.Bytes(u.payload), // slot0 call to the pool contract.
				},
				"latest", // latest signifies the latest block.
			},
			Result: &result,
		}
		pools[i] = pool
	}

	// Batch call to the EVM.
	if err := u.client.BatchCallContext(ctx, batchElems); err != nil {
		u.logger.Error(
			"failed to batch call to ethereum network for all tickers",
			zap.Error(err),
		)

		return types.NewPriceResponseWithErr(
			tickers,
			providertypes.NewErrorWithCode(err, providertypes.ErrorAPIGeneral),
		)
	}

	// Parse the result from the batch call for each ticker.
	for i, ticker := range tickers {
		result := batchElems[i]
		if result.Error != nil {
			u.logger.Error(
				"failed to batch call to ethereum network for ticker",
				zap.String("ticker", ticker.String()),
				zap.Error(result.Error),
			)

			unResolved[ticker] = providertypes.UnresolvedResult{
				ErrorWithCode: providertypes.NewErrorWithCode(
					result.Error,
					providertypes.ErrorUnknown,
				),
			}

			continue
		}

		// Parse the sqrtPriceX96 from the result.
		sqrtPriceX96, err := u.ParseSqrtPriceX96(result.Result)
		if err != nil {
			u.logger.Error(
				"failed to parse sqrt price x96",
				zap.String("ticker", ticker.String()),
				zap.Error(err),
			)

			unResolved[ticker] = providertypes.UnresolvedResult{
				ErrorWithCode: providertypes.NewErrorWithCode(
					err,
					providertypes.ErrorFailedToParsePrice,
				),
			}

			continue
		}

		// Convert the sqrtPriceX96 to a price. This is the raw, unscaled price.
		price := ConvertSquareRootX96Price(sqrtPriceX96)

		// Scale the price to the respective token decimals.
		scaledPrice := ScalePrice(ticker, pools[i], price)
		intPrice, _ := scaledPrice.Int(nil)
		resolved[ticker] = types.NewPriceResult(intPrice, time.Now())
	}

	// Add the price to the resolved prices.
	return types.NewPriceResponse(resolved, unResolved)
}

// GetPool returns the uniswap pool for the given ticker. This will unmarshal the metadata
// and validate the pool config which contains all required information to query the EVM.
func (u *UniswapV3PriceFetcher) GetPool(
	ticker mmtypes.Ticker,
) (PoolConfig, error) {
	if pool, ok := u.poolCache[ticker]; ok {
		return pool, nil
	}

	var cfg PoolConfig
	if err := json.Unmarshal([]byte(ticker.Metadata_JSON), &cfg); err != nil {
		return cfg, fmt.Errorf("failed to unmarshal pool config on ticker: %w", err)
	}
	if err := cfg.ValidateBasic(); err != nil {
		return cfg, fmt.Errorf("invalid ticker pool config: %w", err)
	}

	u.poolCache[ticker] = cfg
	return cfg, nil
}

// ParseSqrtPriceX96 parses the sqrtPriceX96 from the result of the batch call.
func (u *UniswapV3PriceFetcher) ParseSqrtPriceX96(
	result interface{},
) (*big.Int, error) {
	r, ok := result.(*string)
	if !ok {
		return nil, fmt.Errorf("expected result to be a string, got %T", result)
	}

	if r == nil {
		return nil, fmt.Errorf("result is nil")
	}

	bz, err := hexutil.Decode(*r)
	if err != nil {
		return nil, fmt.Errorf("failed to decode hex result: %w", err)
	}

	out, err := u.abi.Methods["slot0"].Outputs.UnpackValues(bz)
	if err != nil {
		return nil, fmt.Errorf("failed to unpack values: %w", err)
	}

	// Parse the sqrtPriceX96 from the result.
	sqrtPriceX96 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)
	return sqrtPriceX96, nil
}
