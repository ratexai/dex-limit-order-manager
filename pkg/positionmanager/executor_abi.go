package positionmanager

import (
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

// SwapExecutorABI is the ABI of the SwapExecutor contract.
const swapExecutorABIJSON = `[
  {
    "inputs": [
      {"internalType": "address", "name": "_keeper", "type": "address"},
      {"internalType": "address", "name": "_swapRouter", "type": "address"},
      {"internalType": "address", "name": "_feeCollector", "type": "address"}
    ],
    "stateMutability": "nonpayable",
    "type": "constructor"
  },
  {
    "inputs": [
      {"internalType": "address", "name": "user", "type": "address"},
      {"internalType": "address", "name": "tokenIn", "type": "address"},
      {"internalType": "address", "name": "tokenOut", "type": "address"},
      {"internalType": "uint24", "name": "poolFee", "type": "uint24"},
      {"internalType": "uint256", "name": "amountIn", "type": "uint256"},
      {"internalType": "uint256", "name": "minAmountOut", "type": "uint256"},
      {"internalType": "uint16", "name": "feeBps", "type": "uint16"}
    ],
    "name": "executeSwap",
    "outputs": [
      {"internalType": "uint256", "name": "amountOut", "type": "uint256"}
    ],
    "stateMutability": "nonpayable",
    "type": "function"
  },
  {
    "anonymous": false,
    "inputs": [
      {"indexed": true, "internalType": "address", "name": "user", "type": "address"},
      {"indexed": true, "internalType": "address", "name": "tokenIn", "type": "address"},
      {"indexed": true, "internalType": "address", "name": "tokenOut", "type": "address"},
      {"indexed": false, "internalType": "uint256", "name": "amountIn", "type": "uint256"},
      {"indexed": false, "internalType": "uint256", "name": "amountOut", "type": "uint256"},
      {"indexed": false, "internalType": "uint256", "name": "feeAmount", "type": "uint256"},
      {"indexed": false, "internalType": "uint16", "name": "feeBps", "type": "uint16"}
    ],
    "name": "SwapExecuted",
    "type": "event"
  }
]`

// parsedSwapExecutorABI is the parsed ABI.
var parsedSwapExecutorABI abi.ABI

func init() {
	var err error
	parsedSwapExecutorABI, err = abi.JSON(strings.NewReader(swapExecutorABIJSON))
	if err != nil {
		panic("failed to parse SwapExecutor ABI: " + err.Error())
	}
}
