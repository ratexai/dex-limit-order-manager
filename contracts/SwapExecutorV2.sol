// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

/// @title IPermit2
/// @notice Minimal interface for Uniswap's canonical Permit2 contract.
interface IPermit2 {
    /// @notice Allowance transfer types.
    struct PermitDetails {
        address token;
        uint160 amount;
        uint48 expiration;
        uint48 nonce;
    }

    struct PermitSingle {
        PermitDetails details;
        address spender;
        uint256 sigDeadline;
    }

    /// @notice Set allowance via signed permit (AllowanceTransfer mode).
    function permit(
        address owner,
        PermitSingle memory permitSingle,
        bytes calldata signature
    ) external;

    /// @notice Transfer tokens using previously-set allowance (AllowanceTransfer mode).
    function transferFrom(
        address from,
        address to,
        uint160 amount,
        address token
    ) external;

    /// @notice SignatureTransfer types.
    struct TokenPermissions {
        address token;
        uint256 amount;
    }

    struct PermitTransferFrom {
        TokenPermissions permitted;
        uint256 nonce;
        uint256 deadline;
    }

    struct SignatureTransferDetails {
        address to;
        uint256 requestedAmount;
    }

    /// @notice Single-use transfer via signature (SignatureTransfer mode).
    function permitTransferFrom(
        PermitTransferFrom memory permit,
        SignatureTransferDetails calldata transferDetails,
        address owner,
        bytes calldata signature
    ) external;

    /// @notice Read current allowance.
    function allowance(
        address user,
        address token,
        address spender
    ) external view returns (uint160 amount, uint48 expiration, uint48 nonce);
}

/// @title ISwapRouter
/// @notice Uniswap V3 / PancakeSwap V3 compatible swap router interface.
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

/// @title SwapExecutorV2
/// @notice Non-custodial keeper-driven swap executor with Permit2 integration.
///         Supports two token-pull modes:
///         1. Legacy: direct ERC20 transferFrom (user approved this contract)
///         2. Permit2 AllowanceTransfer: for position-based multi-level execution
///         3. Permit2 SignatureTransfer: for single-use market swaps
///
///         In all cases, swap output goes directly to the user (recipient = user).
///         Platform fee is deducted from amountIn before the swap.
///
/// @dev    Deployed per-chain with chain-specific swapRouter address.
///         Works with Uniswap V3 (ETH, Base) and PancakeSwap V3 (BSC).
///         Supports native ETH via WETH wrapping for gas-token swaps.

interface IWETH {
    function deposit() external payable;
    function withdraw(uint256) external;
}

contract SwapExecutorV2 is ReentrancyGuard {
    using SafeERC20 for IERC20;

    // ─── State ────────────────────────────────────────────────────────

    address public owner;
    mapping(address => bool) public keepers;

    address public immutable swapRouter;
    address public immutable feeCollector;
    address public immutable permit2;
    address public immutable weth; // WETH address for native ETH wrapping.

    uint256 public constant MAX_FEE_BPS = 500; // 5% hard cap

    // ─── Events ───────────────────────────────────────────────────────

    event SwapExecuted(
        address indexed user,
        address indexed tokenIn,
        address indexed tokenOut,
        uint256 amountIn,
        uint256 amountOut,
        uint256 feeAmount,
        uint16 feeBps
    );

    event PermitActivated(
        address indexed user,
        address indexed token,
        uint160 amount,
        uint48 expiration
    );

    event KeeperUpdated(address indexed keeper, bool authorized);
    event OwnerTransferred(address indexed previousOwner, address indexed newOwner);

    // ─── Errors ───────────────────────────────────────────────────────

    error Unauthorized();
    error FeeTooHigh(uint16 feeBps, uint256 maxFeeBps);
    error ZeroAmount();
    error ZeroAddress();

    // ─── Modifiers ────────────────────────────────────────────────────

    modifier onlyOwner() {
        if (msg.sender != owner) revert Unauthorized();
        _;
    }

    modifier onlyKeeper() {
        if (!keepers[msg.sender]) revert Unauthorized();
        _;
    }

    // ─── Constructor ──────────────────────────────────────────────────

    constructor(
        address _keeper,
        address _swapRouter,
        address _feeCollector,
        address _permit2,
        address _weth
    ) {
        if (_keeper == address(0) || _swapRouter == address(0) ||
            _feeCollector == address(0) || _permit2 == address(0) ||
            _weth == address(0)) {
            revert ZeroAddress();
        }
        owner = msg.sender;
        keepers[_keeper] = true;
        swapRouter = _swapRouter;
        feeCollector = _feeCollector;
        permit2 = _permit2;
        weth = _weth;

        emit KeeperUpdated(_keeper, true);
    }

    /// @notice Accept native ETH (for WETH wrapping or refunds).
    receive() external payable {}

    // ─── Admin ────────────────────────────────────────────────────────

    function setKeeper(address _keeper, bool _authorized) external onlyOwner {
        if (_keeper == address(0)) revert ZeroAddress();
        keepers[_keeper] = _authorized;
        emit KeeperUpdated(_keeper, _authorized);
    }

    function transferOwnership(address _newOwner) external onlyOwner {
        if (_newOwner == address(0)) revert ZeroAddress();
        emit OwnerTransferred(owner, _newOwner);
        owner = _newOwner;
    }

    /// @notice Rescue ETH accidentally sent to the contract.
    function rescueETH(address payable _to) external onlyOwner {
        if (_to == address(0)) revert ZeroAddress();
        (bool sent, ) = _to.call{value: address(this).balance}("");
        require(sent, "ETH transfer failed");
    }

    /// @notice Rescue ERC20 tokens accidentally sent to the contract.
    function rescueToken(address _token, address _to) external onlyOwner {
        if (_to == address(0)) revert ZeroAddress();
        uint256 balance = IERC20(_token).balanceOf(address(this));
        if (balance > 0) {
            IERC20(_token).safeTransfer(_to, balance);
        }
    }

    // ─── Legacy: Direct ERC20 approve ─────────────────────────────────

    /// @notice Execute swap using direct ERC20 transferFrom (backward compat).
    ///         User must have approved this contract for tokenIn.
    function executeSwap(
        address user,
        address tokenIn,
        address tokenOut,
        uint24 poolFee,
        uint256 amountIn,
        uint256 minAmountOut,
        uint16 feeBps
    ) external nonReentrant onlyKeeper returns (uint256 amountOut) {
        if (amountIn == 0) revert ZeroAmount();
        if (feeBps > MAX_FEE_BPS) revert FeeTooHigh(feeBps, MAX_FEE_BPS);

        // Pull tokens from user via direct ERC20 approval.
        IERC20(tokenIn).safeTransferFrom(user, address(this), amountIn);

        return _swapAndEmit(user, tokenIn, tokenOut, poolFee, amountIn, minAmountOut, feeBps);
    }

    // ─── Permit2 AllowanceTransfer: Multi-level positions ─────────────

    /// @notice Activate a Permit2 allowance using a user's pre-signed permit.
    ///         Called once per position before the first level execution.
    ///         The permit grants this contract an allowance on Permit2 for
    ///         the position's total size with an expiry.
    function activatePermit(
        address user,
        IPermit2.PermitSingle calldata permitSingle,
        bytes calldata signature
    ) external onlyKeeper {
        IPermit2(permit2).permit(user, permitSingle, signature);

        emit PermitActivated(
            user,
            permitSingle.details.token,
            permitSingle.details.amount,
            permitSingle.details.expiration
        );
    }

    /// @notice Execute swap using Permit2 AllowanceTransfer.
    ///         Requires prior activatePermit() call to set the allowance.
    ///         Each call decrements the Permit2 allowance by amountIn.
    function executeSwapViaPermit2(
        address user,
        address tokenIn,
        address tokenOut,
        uint24 poolFee,
        uint256 amountIn,
        uint256 minAmountOut,
        uint16 feeBps
    ) external nonReentrant onlyKeeper returns (uint256 amountOut) {
        if (amountIn == 0) revert ZeroAmount();
        if (feeBps > MAX_FEE_BPS) revert FeeTooHigh(feeBps, MAX_FEE_BPS);

        // Pull tokens from user via Permit2 allowance (decrements allowance).
        IPermit2(permit2).transferFrom(
            user,
            address(this),
            uint160(amountIn),
            tokenIn
        );

        return _swapAndEmit(user, tokenIn, tokenOut, poolFee, amountIn, minAmountOut, feeBps);
    }

    // ─── Permit2 SignatureTransfer: Single-use market swaps ───────────

    /// @notice Execute swap using Permit2 SignatureTransfer (single-use).
    ///         Used for market swaps where user signs a one-time permit.
    ///         The permit nonce is consumed atomically — cannot be replayed.
    function executeSwapWithSignature(
        address user,
        address tokenOut,
        uint24 poolFee,
        uint256 minAmountOut,
        uint16 feeBps,
        IPermit2.PermitTransferFrom calldata permitTransfer,
        bytes calldata signature
    ) external nonReentrant onlyKeeper returns (uint256 amountOut) {
        uint256 amountIn = permitTransfer.permitted.amount;
        address tokenIn = permitTransfer.permitted.token;

        if (amountIn == 0) revert ZeroAmount();
        if (feeBps > MAX_FEE_BPS) revert FeeTooHigh(feeBps, MAX_FEE_BPS);

        // Pull tokens via single-use Permit2 signature (nonce consumed).
        IPermit2(permit2).permitTransferFrom(
            permitTransfer,
            IPermit2.SignatureTransferDetails({
                to: address(this),
                requestedAmount: amountIn
            }),
            user,
            signature
        );

        return _swapAndEmit(user, tokenIn, tokenOut, poolFee, amountIn, minAmountOut, feeBps);
    }

    // ─── Native ETH: Wrap and swap ──────────────────────────────────

    /// @notice Execute swap using native ETH. Wraps ETH → WETH, then swaps.
    ///         User (or keeper relaying) sends raw ETH via msg.value.
    ///         No Permit2 or approve needed — ETH is sent directly.
    function executeSwapNativeETH(
        address user,
        address tokenOut,
        uint24 poolFee,
        uint256 minAmountOut,
        uint16 feeBps
    ) external payable nonReentrant onlyKeeper returns (uint256 amountOut) {
        if (msg.value == 0) revert ZeroAmount();
        if (feeBps > MAX_FEE_BPS) revert FeeTooHigh(feeBps, MAX_FEE_BPS);

        // Wrap ETH → WETH.
        IWETH(weth).deposit{value: msg.value}();

        return _swapAndEmit(user, weth, tokenOut, poolFee, msg.value, minAmountOut, feeBps);
    }

    // ─── Internal: Shared swap logic ──────────────────────────────────

    /// @dev Fee deduction + Uniswap/PancakeSwap V3 swap + emit event.
    ///      Tokens must already be in this contract before calling.
    function _swapAndEmit(
        address user,
        address tokenIn,
        address tokenOut,
        uint24 poolFee,
        uint256 amountIn,
        uint256 minAmountOut,
        uint16 feeBps
    ) internal returns (uint256 amountOut) {
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
