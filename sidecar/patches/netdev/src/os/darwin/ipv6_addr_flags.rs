use std::net::Ipv6Addr;

use crate::interface::ipv6_addr_flags::Ipv6AddrFlags;

// <netinet6/in6_var.h> — not yet in `libc`.
const SIOCGIFAFLAG_IN6: libc::c_ulong = 0xC1206949;
const IN6_IFF_TENTATIVE: u32 = 0x02;
const IN6_IFF_DUPLICATED: u32 = 0x04;
const IN6_IFF_DEPRECATED: u32 = 0x10;
const IN6_IFF_TEMPORARY: u32 = 0x80;

// Kinara patch: `libc` only has `in6_ifreq` for macOS, so this didn't build
// for iPhone. The struct from <netinet6/in6_var.h>, with the union cut down
// to the two members used here and padded to the full size, since the ioctl
// number above encodes it (0x120 bytes) and the kernel copies that much.
#[repr(C)]
#[allow(non_camel_case_types)]
struct in6_ifreq {
    ifr_name: [libc::c_char; libc::IFNAMSIZ],
    ifr_ifru: in6_ifreq_ifru,
}

#[repr(C)]
#[allow(non_camel_case_types)]
union in6_ifreq_ifru {
    ifru_addr: libc::sockaddr_in6,
    ifru_flags6: libc::c_int,
    _size: [u8; 0x120 - libc::IFNAMSIZ],
}

const _: () = assert!(std::mem::size_of::<in6_ifreq>() == 0x120);

pub(crate) fn get_ipv6_addr_flags(ifname: &str, addr: &Ipv6Addr) -> Ipv6AddrFlags {
    unsafe {
        let fd = libc::socket(libc::AF_INET6, libc::SOCK_DGRAM, 0);
        if fd < 0 {
            return Ipv6AddrFlags::default();
        }

        let mut req: in6_ifreq = std::mem::zeroed();

        let name_bytes = ifname.as_bytes();
        let copy_len = name_bytes.len().min(libc::IFNAMSIZ - 1);
        std::ptr::copy_nonoverlapping(
            name_bytes.as_ptr(),
            req.ifr_name.as_mut_ptr().cast(),
            copy_len,
        );

        req.ifr_ifru.ifru_addr.sin6_family = libc::AF_INET6 as libc::sa_family_t;
        req.ifr_ifru.ifru_addr.sin6_len =
            std::mem::size_of::<libc::sockaddr_in6>() as libc::c_uchar;
        req.ifr_ifru.ifru_addr.sin6_addr.s6_addr = addr.octets();

        let ret = libc::ioctl(fd, SIOCGIFAFLAG_IN6, &mut req);
        libc::close(fd);

        if ret < 0 {
            return Ipv6AddrFlags::default();
        }

        let raw = req.ifr_ifru.ifru_flags6 as u32;

        Ipv6AddrFlags {
            deprecated: raw & IN6_IFF_DEPRECATED != 0,
            temporary: raw & IN6_IFF_TEMPORARY != 0,
            tentative: raw & IN6_IFF_TENTATIVE != 0,
            duplicated: raw & IN6_IFF_DUPLICATED != 0,
            permanent: false,
        }
    }
}
