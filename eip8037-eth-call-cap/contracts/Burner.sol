// SPDX-License-Identifier: AGPL-3.0-only
pragma solidity ^0.8.20;

/// Burns a controllable amount of pure execution gas in a view, with no state
/// writes. The hash input is written to scratch space (0x00..0x3f) so memory
/// does not grow and the cost per iteration is constant.
contract Burner {
    function burn(uint256 n) external pure returns (bytes32 h) {
        for (uint256 i = 0; i < n; i++) {
            assembly {
                mstore(0x00, h)
                mstore(0x20, i)
                h := keccak256(0x00, 0x40)
            }
        }
    }
}
