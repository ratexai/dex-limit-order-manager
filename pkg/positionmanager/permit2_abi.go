package positionmanager

import (
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

// Permit2 canonical address — same on all EVM chains (CREATE2 deployment).
const Permit2CanonicalAddress = "0x000000000022D473030F116dDEE9F6B43aC78BA3"

// permit2ABIJSON contains the minimal Permit2 ABI needed for AllowanceTransfer
// and SignatureTransfer modes.
const permit2ABIJSON = `[
  {
    "inputs": [
      {
        "internalType": "address",
        "name": "owner",
        "type": "address"
      },
      {
        "components": [
          {
            "components": [
              {"internalType": "address", "name": "token", "type": "address"},
              {"internalType": "uint160", "name": "amount", "type": "uint160"},
              {"internalType": "uint48", "name": "expiration", "type": "uint48"},
              {"internalType": "uint48", "name": "nonce", "type": "uint48"}
            ],
            "internalType": "struct IAllowanceTransfer.PermitDetails",
            "name": "details",
            "type": "tuple"
          },
          {"internalType": "address", "name": "spender", "type": "address"},
          {"internalType": "uint256", "name": "sigDeadline", "type": "uint256"}
        ],
        "internalType": "struct IAllowanceTransfer.PermitSingle",
        "name": "permitSingle",
        "type": "tuple"
      },
      {
        "internalType": "bytes",
        "name": "signature",
        "type": "bytes"
      }
    ],
    "name": "permit",
    "outputs": [],
    "stateMutability": "nonpayable",
    "type": "function"
  },
  {
    "inputs": [
      {"internalType": "address", "name": "from", "type": "address"},
      {"internalType": "address", "name": "to", "type": "address"},
      {"internalType": "uint160", "name": "amount", "type": "uint160"},
      {"internalType": "address", "name": "token", "type": "address"}
    ],
    "name": "transferFrom",
    "outputs": [],
    "stateMutability": "nonpayable",
    "type": "function"
  },
  {
    "inputs": [
      {"internalType": "address", "name": "user", "type": "address"},
      {"internalType": "address", "name": "token", "type": "address"},
      {"internalType": "address", "name": "spender", "type": "address"}
    ],
    "name": "allowance",
    "outputs": [
      {"internalType": "uint160", "name": "amount", "type": "uint160"},
      {"internalType": "uint48", "name": "expiration", "type": "uint48"},
      {"internalType": "uint48", "name": "nonce", "type": "uint48"}
    ],
    "stateMutability": "view",
    "type": "function"
  }
]`

// swapExecutorV2ABIJSON is the ABI for SwapExecutorV2 with Permit2 integration.
const swapExecutorV2ABIJSON = `[
  {
    "inputs": [
      {"internalType": "address", "name": "_keeper", "type": "address"},
      {"internalType": "address", "name": "_swapRouter", "type": "address"},
      {"internalType": "address", "name": "_feeCollector", "type": "address"},
      {"internalType": "address", "name": "_permit2", "type": "address"}
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
    "inputs": [
      {"internalType": "address", "name": "user", "type": "address"},
      {
        "components": [
          {
            "components": [
              {"internalType": "address", "name": "token", "type": "address"},
              {"internalType": "uint160", "name": "amount", "type": "uint160"},
              {"internalType": "uint48", "name": "expiration", "type": "uint48"},
              {"internalType": "uint48", "name": "nonce", "type": "uint48"}
            ],
            "internalType": "struct IPermit2.PermitDetails",
            "name": "details",
            "type": "tuple"
          },
          {"internalType": "address", "name": "spender", "type": "address"},
          {"internalType": "uint256", "name": "sigDeadline", "type": "uint256"}
        ],
        "internalType": "struct IPermit2.PermitSingle",
        "name": "permitSingle",
        "type": "tuple"
      },
      {"internalType": "bytes", "name": "signature", "type": "bytes"}
    ],
    "name": "activatePermit",
    "outputs": [],
    "stateMutability": "nonpayable",
    "type": "function"
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
    "name": "executeSwapViaPermit2",
    "outputs": [
      {"internalType": "uint256", "name": "amountOut", "type": "uint256"}
    ],
    "stateMutability": "nonpayable",
    "type": "function"
  },
  {
    "inputs": [
      {"internalType": "address", "name": "user", "type": "address"},
      {"internalType": "address", "name": "tokenOut", "type": "address"},
      {"internalType": "uint24", "name": "poolFee", "type": "uint24"},
      {"internalType": "uint256", "name": "minAmountOut", "type": "uint256"},
      {"internalType": "uint16", "name": "feeBps", "type": "uint16"},
      {
        "components": [
          {
            "components": [
              {"internalType": "address", "name": "token", "type": "address"},
              {"internalType": "uint256", "name": "amount", "type": "uint256"}
            ],
            "internalType": "struct IPermit2.TokenPermissions",
            "name": "permitted",
            "type": "tuple"
          },
          {"internalType": "uint256", "name": "nonce", "type": "uint256"},
          {"internalType": "uint256", "name": "deadline", "type": "uint256"}
        ],
        "internalType": "struct IPermit2.PermitTransferFrom",
        "name": "permitTransfer",
        "type": "tuple"
      },
      {"internalType": "bytes", "name": "signature", "type": "bytes"}
    ],
    "name": "executeSwapWithSignature",
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
  },
  {
    "anonymous": false,
    "inputs": [
      {"indexed": true, "internalType": "address", "name": "user", "type": "address"},
      {"indexed": true, "internalType": "address", "name": "token", "type": "address"},
      {"indexed": false, "internalType": "uint160", "name": "amount", "type": "uint160"},
      {"indexed": false, "internalType": "uint48", "name": "expiration", "type": "uint48"}
    ],
    "name": "PermitActivated",
    "type": "event"
  }
]`

var (
	parsedPermit2ABI         abi.ABI
	parsedSwapExecutorV2ABI  abi.ABI
)

func init() {
	var err error
	parsedPermit2ABI, err = abi.JSON(strings.NewReader(permit2ABIJSON))
	if err != nil {
		panic("failed to parse Permit2 ABI: " + err.Error())
	}

	parsedSwapExecutorV2ABI, err = abi.JSON(strings.NewReader(swapExecutorV2ABIJSON))
	if err != nil {
		panic("failed to parse SwapExecutorV2 ABI: " + err.Error())
	}
}
