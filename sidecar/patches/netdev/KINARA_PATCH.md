# Kinara patch

This is netdev 0.46.3 from crates.io with two changes, for Apple devices:

* **iPhone:** `libc` only defines `in6_ifreq` for macOS, so netdev didn't
  build for iOS. It's defined here instead, in
  `src/os/darwin/ipv6_addr_flags.rs` (marked "Kinara patch").
* **Apple TV:** every place netdev picks its iOS code also picks it on tvOS,
  made by `patches/add-tvos.py`. tvOS has the same APIs for what netdev uses
  (interfaces, routes, Network.framework's path monitor from tvOS 12).
  Without it netdev builds with no platform code on Apple TV.

Drop this copy (and its line under [patch.crates-io]) once a netdev release
has both and iroh's netwatch uses it.
