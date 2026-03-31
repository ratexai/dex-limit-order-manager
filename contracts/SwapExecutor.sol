// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

/// @title SwapExecutor
/// @notice Thin executor contract for keeper-driven swaps on Uniswap V3.
///         Non-custodial: tokens transit through the contract in a single TX,
///         swap result goes directly to the user. Platform fee is deducted
///         from amountIn before the swap and sent to feeCollector.
/// @dev    User must approve tokenIn to this contract once.
///         Only the authorized keeper address can call executeSwap.
interface ISwapRouter {
    struct ExactInputSingleParams {
        address tokenIn;
        address tokenOut;
        uint24 fee;
        address recipient;
        uint256 amountIn;
        uint256 amountOutMinimum;
        uint160 sqrtPriceLimitX96;
    }

    function exactInputSingle(
        ExactInputSingleParams calldata params
    ) external returns (uint256 amountOut);
}

contract SwapExecutor is ReentrancyGuard {
    using SafeERC20 for IERC20;

    /// @notice Authorized keeper address (set at deploy, immutable).
    address public immutable keeper;

    /// @notice Uniswap V3 SwapRouter02 address (set at deploy, immutable).
    address public immutable swapRouter;

    /// @notice Treasury address that receives platform fees (set at deploy, immutable).
    address public immutable feeCollector;

    /// @notice Hard cap on fee in basis points (5%). Protects against misconfiguration.
    uint256 public constant MAX_FEE_BPS = 500;

    event SwapExecuted(
        address indexed user,
        address indexed tokenIn,
        address indexed tokenOut,
        uint256 amountIn,
        uint256 amountOut,
        uint256 feeAmount,
        uint16 feeBps
    );

    error Unauthorized();
    error FeeTooHigh(uint16 feeBps, uint256 maxFeeBps);
    error ZeroAmount();
    error ZeroAddress();

    constructor(address _keeper, address _swapRouter, address _feeCollector) {
        if (_keeper == address(0) || _swapRouter == address(0) || _feeCollector == address(0)) {
            revert ZeroAddress();
        }
        keeper = _keeper;
        swapRouter = _swapRouter;
        feeCollector = _feeCollector;
    }

    /// @notice Execute a swap on behalf of a user via Uniswap V3.
    /// @param user         The user whose tokens will be swapped. Must have approved this contract.
    /// @param tokenIn      Token to sell.
    /// @param tokenOut     Token to buy.
    /// @param poolFee      Uniswap V3 pool fee tier (500, 3000, 10000).
    /// @param amountIn     Total amount of tokenIn to pull from user (fee is deducted from this).
    /// @param minAmountOut Minimum acceptable amount of tokenOut (slippage protection).
    /// @param feeBps       Platform fee in basis points (100 = 1%). 0 = no fee.
    ///                     Different users have different fee tiers; keeper passes the rate.
    /// @return amountOut   Actual amount of tokenOut received by the user.
    function executeSwap(
        address user,
        address tokenIn,
        address tokenOut,
        uint24 poolFee,
        uint256 amountIn,
        uint256 minAmountOut,
        uint16 feeBps
    ) external nonReentrant returns (uint256 amountOut) {
        if (msg.sender != keeper) revert Unauthorized();
        if (amountIn == 0) revert ZeroAmount();
        if (feeBps > MAX_FEE_BPS) revert FeeTooHigh(feeBps, MAX_FEE_BPS);

        // Pull tokens from user.
        IERC20(tokenIn).safeTransferFrom(user, address(this), amountIn);

        // Deduct platform fee before swap.
        uint256 feeAmount;
        uint256 swapAmount = amountIn;
        if (feeBps > 0) {
            feeAmount = (amountIn * feeBps) / 10000;
            swapAmount = amountIn - feeAmount;
            IERC20(tokenIn).safeTransfer(feeCollector, feeAmount);
        }

        // Approve router for exact swap amount.
        IERC20(tokenIn).forceApprove(swapRouter, swapAmount);

        // Execute swap — result goes directly to user.
        amountOut = ISwapRouter(swapRouter).exactInputSingle(
            ISwapRouter.ExactInputSingleParams({
                tokenIn: tokenIn,
                tokenOut: tokenOut,
                fee: poolFee,
                recipient: user,
                amountIn: swapAmount,
                amountOutMinimum: minAmountOut,
                sqrtPriceLimitX96: 0
            })
        );

        emit SwapExecuted(
            user,
            tokenIn,
            tokenOut,
            amountIn,
            amountOut,
            feeAmount,
            feeBps
        );
    }
}
