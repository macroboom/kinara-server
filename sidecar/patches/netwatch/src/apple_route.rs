//! Kinara patch: the routing-socket constants on iPhone and Apple TV.
//!
//! libc only defines these (from Apple's <net/route.h>) for some Apple
//! systems, so netwatch's route code didn't build for tvOS. The values are
//! the same on every Apple system: macOS takes them from libc as before, and
//! checks there that the copies below match; iOS and tvOS use the copies.

#![allow(dead_code)]

use libc::c_int;

mod values {
    use super::c_int;

    pub const RTF_GATEWAY: c_int = 0x2;
    pub const RTF_IFSCOPE: c_int = 0x1000000;

    pub const RTA_IFP: c_int = 0x10;

    pub const RTAX_DST: c_int = 0;
    pub const RTAX_GATEWAY: c_int = 1;
    pub const RTAX_NETMASK: c_int = 2;
    pub const RTAX_IFP: c_int = 4;
    pub const RTAX_BRD: c_int = 7;
    pub const RTAX_MAX: c_int = 8;

    pub const RTM_VERSION: c_int = 5;
    pub const RTM_ADD: c_int = 0x1;
    pub const RTM_DELETE: c_int = 0x2;
    pub const RTM_CHANGE: c_int = 0x3;
    pub const RTM_GET: c_int = 0x4;
    pub const RTM_LOSING: c_int = 0x5;
    pub const RTM_REDIRECT: c_int = 0x6;
    pub const RTM_MISS: c_int = 0x7;
    pub const RTM_LOCK: c_int = 0x8;
    pub const RTM_RESOLVE: c_int = 0xb;
    pub const RTM_NEWADDR: c_int = 0xc;
    pub const RTM_DELADDR: c_int = 0xd;
    pub const RTM_IFINFO: c_int = 0xe;
    pub const RTM_NEWMADDR: c_int = 0xf;
    pub const RTM_DELMADDR: c_int = 0x10;
    pub const RTM_IFINFO2: c_int = 0x12;
    pub const RTM_NEWMADDR2: c_int = 0x13;
    pub const RTM_GET2: c_int = 0x14;
}

#[cfg(target_os = "macos")]
pub(crate) use libc::{
    RTA_IFP, RTAX_BRD, RTAX_DST, RTAX_GATEWAY, RTAX_IFP, RTAX_MAX, RTAX_NETMASK, RTF_GATEWAY,
    RTF_IFSCOPE, RTM_ADD, RTM_CHANGE, RTM_DELADDR, RTM_DELETE, RTM_DELMADDR, RTM_GET, RTM_GET2,
    RTM_IFINFO, RTM_IFINFO2, RTM_LOCK, RTM_LOSING, RTM_MISS, RTM_NEWADDR, RTM_NEWMADDR,
    RTM_NEWMADDR2, RTM_REDIRECT, RTM_RESOLVE, RTM_VERSION,
};

#[cfg(not(target_os = "macos"))]
pub(crate) use self::values::*;

// On macOS, where libc has them all: the copies must match.
#[cfg(target_os = "macos")]
const _: () = {
    assert!(values::RTF_GATEWAY == libc::RTF_GATEWAY);
    assert!(values::RTF_IFSCOPE == libc::RTF_IFSCOPE);
    assert!(values::RTA_IFP == libc::RTA_IFP);
    assert!(values::RTAX_DST == libc::RTAX_DST);
    assert!(values::RTAX_GATEWAY == libc::RTAX_GATEWAY);
    assert!(values::RTAX_NETMASK == libc::RTAX_NETMASK);
    assert!(values::RTAX_IFP == libc::RTAX_IFP);
    assert!(values::RTAX_BRD == libc::RTAX_BRD);
    assert!(values::RTAX_MAX == libc::RTAX_MAX);
    assert!(values::RTM_VERSION == libc::RTM_VERSION);
    assert!(values::RTM_ADD == libc::RTM_ADD);
    assert!(values::RTM_DELETE == libc::RTM_DELETE);
    assert!(values::RTM_CHANGE == libc::RTM_CHANGE);
    assert!(values::RTM_GET == libc::RTM_GET);
    assert!(values::RTM_LOSING == libc::RTM_LOSING);
    assert!(values::RTM_REDIRECT == libc::RTM_REDIRECT);
    assert!(values::RTM_MISS == libc::RTM_MISS);
    assert!(values::RTM_LOCK == libc::RTM_LOCK);
    assert!(values::RTM_RESOLVE == libc::RTM_RESOLVE);
    assert!(values::RTM_NEWADDR == libc::RTM_NEWADDR);
    assert!(values::RTM_DELADDR == libc::RTM_DELADDR);
    assert!(values::RTM_IFINFO == libc::RTM_IFINFO);
    assert!(values::RTM_NEWMADDR == libc::RTM_NEWMADDR);
    assert!(values::RTM_DELMADDR == libc::RTM_DELMADDR);
    assert!(values::RTM_IFINFO2 == libc::RTM_IFINFO2);
    assert!(values::RTM_NEWMADDR2 == libc::RTM_NEWMADDR2);
    assert!(values::RTM_GET2 == libc::RTM_GET2);
};
