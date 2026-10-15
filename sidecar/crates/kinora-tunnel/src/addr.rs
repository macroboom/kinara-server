//! Address classification for status output and path typing.

use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};

/// Folds IPv4-mapped IPv6 addresses back to IPv4. Dual-stack sockets report
/// IPv4 peers this way, and we want them counted as IPv4.
pub fn canonical(addr: SocketAddr) -> SocketAddr {
    match addr {
        SocketAddr::V6(v6) => match v6.ip().to_ipv4_mapped() {
            Some(v4) => SocketAddr::new(IpAddr::V4(v4), v6.port()),
            None => addr,
        },
        SocketAddr::V4(_) => addr,
    }
}

pub fn family(addr: SocketAddr) -> &'static str {
    match canonical(addr) {
        SocketAddr::V4(_) => "ipv4",
        SocketAddr::V6(_) => "ipv6",
    }
}

/// One of "global", "lan", "link-local", "loopback".
pub fn scope(ip: IpAddr) -> &'static str {
    match ip {
        IpAddr::V4(v4) => scope_v4(v4),
        IpAddr::V6(v6) => match v6.to_ipv4_mapped() {
            Some(v4) => scope_v4(v4),
            None => scope_v6(v6),
        },
    }
}

fn scope_v4(ip: Ipv4Addr) -> &'static str {
    let o = ip.octets();
    if ip.is_loopback() {
        "loopback"
    } else if ip.is_link_local() {
        "link-local"
    } else if ip.is_private()
        || ip.is_unspecified()
        // 100.64.0.0/10 is carrier-grade NAT space, never reachable from outside.
        || (o[0] == 100 && (o[1] & 0xc0) == 64)
    {
        "lan"
    } else {
        "global"
    }
}

fn scope_v6(ip: Ipv6Addr) -> &'static str {
    let first = ip.segments()[0];
    if ip.is_loopback() {
        "loopback"
    } else if (first & 0xffc0) == 0xfe80 {
        "link-local"
    } else if (first & 0xe000) == 0x2000 {
        // 2000::/3 is the only block handed out as global unicast.
        "global"
    } else {
        // fc00::/7 unique local, fec0::/10 site local, and anything odd.
        "lan"
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ip(s: &str) -> IpAddr {
        s.parse().unwrap()
    }

    #[test]
    fn scopes() {
        assert_eq!(scope(ip("8.8.8.8")), "global");
        assert_eq!(scope(ip("192.168.1.20")), "lan");
        assert_eq!(scope(ip("10.0.0.5")), "lan");
        assert_eq!(scope(ip("100.100.1.1")), "lan");
        assert_eq!(scope(ip("169.254.3.4")), "link-local");
        assert_eq!(scope(ip("127.0.0.1")), "loopback");
        assert_eq!(scope(ip("2601:1c2:4000::17")), "global");
        assert_eq!(scope(ip("fd12:3456::1")), "lan");
        assert_eq!(scope(ip("fe80::1")), "link-local");
        assert_eq!(scope(ip("::1")), "loopback");
        assert_eq!(scope(ip("::ffff:192.168.1.2")), "lan");
    }

    #[test]
    fn canonical_unmaps_v4() {
        let mapped: SocketAddr = "[::ffff:203.0.113.9]:4433".parse().unwrap();
        assert_eq!(canonical(mapped), "203.0.113.9:4433".parse::<SocketAddr>().unwrap());
        assert_eq!(family(mapped), "ipv4");
        let v6: SocketAddr = "[2001:db8::1]:1".parse().unwrap();
        assert_eq!(canonical(v6), v6);
        assert_eq!(family(v6), "ipv6");
    }
}
