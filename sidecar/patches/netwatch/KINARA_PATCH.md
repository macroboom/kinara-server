# Kinara patch

This is netwatch 0.19.3 from crates.io with two changes, for Apple TV:

* **tvOS next to iOS:** every place it picks iOS code also picks it on Apple
  TV, made by `patches/add-tvos.py`. tvOS has the same system APIs for what
  netwatch uses. Without it netwatch builds with no platform code there.
* **Route constants:** libc doesn't define the routing-socket constants
  (`RTM_*`, `RTAX_*`, `RTF_GATEWAY`, `RTF_IFSCOPE`, `RTA_IFP`) for tvOS.
  `src/apple_route.rs` has Apple's values from <net/route.h>, the same on
  every Apple system; the route code takes them from there (marked
  "Kinara patch"). On macOS it still uses libc's, and a compile-time check
  makes sure the copies match.

Re-running add-tvos.py on this folder is safe: it leaves both changes alone.
Drop this copy (and its line under [patch.crates-io]) once a netwatch
release supports tvOS.
