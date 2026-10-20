//! PROXY protocol v2 header. The sidecar writes one at the start of every TCP
//! connection it opens to the Go server's tunnel listener, so the Go side
//! learns which remote device the bytes belong to.
//!
//! Spec: https://www.haproxy.org/download/3.0/doc/proxy-protocol.txt

use std::fmt;
use std::net::{Ipv4Addr, Ipv6Addr, SocketAddr, SocketAddrV4, SocketAddrV6};

use crate::addr::canonical;

pub const SIGNATURE: [u8; 12] = [0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A];
/// Version 2, command PROXY.
pub const VER_CMD_PROXY: u8 = 0x21;
/// Version 2, command LOCAL. Only accepted when parsing.
pub const VER_CMD_LOCAL: u8 = 0x20;
pub const FAM_UNSPEC: u8 = 0x00;
pub const FAM_TCP4: u8 = 0x11;
pub const FAM_TCP6: u8 = 0x21;
const FAM_UNIX_STREAM: u8 = 0x31;
/// Peer iroh node ID, lowercase hex (64 ASCII bytes).
pub const TLV_NODE_ID: u8 = 0xE0;
/// Path type as ASCII: direct-v4, direct-v6, relay, mixed.
pub const TLV_PATH: u8 = 0xE1;
/// Shared secret from kinara-server (KINARA_IROH_SECRET), so the Go side can
/// tell the sidecar's connections from any other local program's.
pub const TLV_SECRET: u8 = 0xE2;
/// Signature, version/command, family and length.
pub const FIXED_LEN: usize = 16;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Addresses {
    Unspec,
    V4 { src: SocketAddrV4, dst: SocketAddrV4 },
    V6 { src: SocketAddrV6, dst: SocketAddrV6 },
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Header {
    pub addresses: Addresses,
    pub tlvs: Vec<(u8, Vec<u8>)>,
}

impl Header {
    /// Builds the header for one tunnelled connection. `peer` is the remote
    /// device's current UDP address, `None` when the traffic is relayed.
    /// `local` is the sidecar's bound UDP socket, used as the destination.
    pub fn for_peer(peer: Option<SocketAddr>, local: Option<SocketAddr>, node_id: &str, path: &str) -> Header {
        let local = local.map(canonical);
        let addresses = match peer.map(canonical) {
            None => Addresses::Unspec,
            Some(SocketAddr::V4(src)) => {
                let dst = match local {
                    Some(SocketAddr::V4(d)) => d,
                    Some(SocketAddr::V6(d)) => SocketAddrV4::new(Ipv4Addr::UNSPECIFIED, d.port()),
                    None => SocketAddrV4::new(Ipv4Addr::UNSPECIFIED, 0),
                };
                Addresses::V4 { src, dst }
            }
            Some(SocketAddr::V6(src)) => {
                // Flow info and scope IDs have no place in the header.
                let src = SocketAddrV6::new(*src.ip(), src.port(), 0, 0);
                let dst = match local {
                    Some(SocketAddr::V6(d)) => SocketAddrV6::new(*d.ip(), d.port(), 0, 0),
                    Some(SocketAddr::V4(d)) => SocketAddrV6::new(d.ip().to_ipv6_mapped(), d.port(), 0, 0),
                    None => SocketAddrV6::new(Ipv6Addr::UNSPECIFIED, 0, 0, 0),
                };
                Addresses::V6 { src, dst }
            }
        };
        Header {
            addresses,
            tlvs: vec![(TLV_NODE_ID, node_id.as_bytes().to_vec()), (TLV_PATH, path.as_bytes().to_vec())],
        }
    }

    pub fn tlv(&self, kind: u8) -> Option<&[u8]> {
        self.tlvs.iter().find(|(k, _)| *k == kind).map(|(_, v)| v.as_slice())
    }

    pub fn node_id(&self) -> Option<&str> {
        self.tlv(TLV_NODE_ID).and_then(|v| std::str::from_utf8(v).ok())
    }

    pub fn path(&self) -> Option<&str> {
        self.tlv(TLV_PATH).and_then(|v| std::str::from_utf8(v).ok())
    }

    /// Adds the shared secret TLV (nothing when there is no secret).
    pub fn with_secret(mut self, secret: Option<&str>) -> Header {
        if let Some(s) = secret.filter(|s| !s.is_empty()) {
            self.tlvs.push((TLV_SECRET, s.as_bytes().to_vec()));
        }
        self
    }

    pub fn secret(&self) -> Option<&str> {
        self.tlv(TLV_SECRET).and_then(|v| std::str::from_utf8(v).ok())
    }

    pub fn source(&self) -> Option<SocketAddr> {
        match &self.addresses {
            Addresses::Unspec => None,
            Addresses::V4 { src, .. } => Some(SocketAddr::V4(*src)),
            Addresses::V6 { src, .. } => Some(SocketAddr::V6(*src)),
        }
    }

    pub fn encode(&self) -> Vec<u8> {
        let mut body = Vec::with_capacity(36 + 80);
        let family = match &self.addresses {
            Addresses::Unspec => FAM_UNSPEC,
            Addresses::V4 { src, dst } => {
                body.extend_from_slice(&src.ip().octets());
                body.extend_from_slice(&dst.ip().octets());
                body.extend_from_slice(&src.port().to_be_bytes());
                body.extend_from_slice(&dst.port().to_be_bytes());
                FAM_TCP4
            }
            Addresses::V6 { src, dst } => {
                body.extend_from_slice(&src.ip().octets());
                body.extend_from_slice(&dst.ip().octets());
                body.extend_from_slice(&src.port().to_be_bytes());
                body.extend_from_slice(&dst.port().to_be_bytes());
                FAM_TCP6
            }
        };
        for (kind, value) in &self.tlvs {
            let len = value.len().min(u16::MAX as usize);
            body.push(*kind);
            body.extend_from_slice(&(len as u16).to_be_bytes());
            body.extend_from_slice(&value[..len]);
        }
        debug_assert!(body.len() <= u16::MAX as usize);
        let mut out = Vec::with_capacity(FIXED_LEN + body.len());
        out.extend_from_slice(&SIGNATURE);
        out.push(VER_CMD_PROXY);
        out.push(family);
        out.extend_from_slice(&(body.len() as u16).to_be_bytes());
        out.extend_from_slice(&body);
        out
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ParseError {
    /// More bytes are needed; `needed` is the total header length if known.
    Incomplete {
        needed: usize,
    },
    BadSignature,
    BadVersion(u8),
    BadFamily(u8),
    BadLength,
    BadTlv,
}

impl fmt::Display for ParseError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            ParseError::Incomplete { needed } => write!(f, "incomplete PROXY header, need {needed} bytes"),
            ParseError::BadSignature => write!(f, "missing PROXY v2 signature"),
            ParseError::BadVersion(b) => write!(f, "unsupported PROXY version/command byte 0x{b:02x}"),
            ParseError::BadFamily(b) => write!(f, "unsupported PROXY family byte 0x{b:02x}"),
            ParseError::BadLength => write!(f, "PROXY length too short for its address family"),
            ParseError::BadTlv => write!(f, "malformed PROXY TLV"),
        }
    }
}

impl std::error::Error for ParseError {}

/// Total header length once the first 16 bytes are known.
pub fn header_len(fixed: &[u8]) -> Result<usize, ParseError> {
    if fixed.len() < FIXED_LEN {
        return Err(ParseError::Incomplete { needed: FIXED_LEN });
    }
    if fixed[..12] != SIGNATURE {
        return Err(ParseError::BadSignature);
    }
    Ok(FIXED_LEN + u16::from_be_bytes([fixed[14], fixed[15]]) as usize)
}

/// Parses a header from the front of `buf`. Returns the header and how many
/// bytes it used.
pub fn parse(buf: &[u8]) -> Result<(Header, usize), ParseError> {
    let total = header_len(buf)?;
    if buf.len() < total {
        return Err(ParseError::Incomplete { needed: total });
    }
    let ver_cmd = buf[12];
    if ver_cmd != VER_CMD_PROXY && ver_cmd != VER_CMD_LOCAL {
        return Err(ParseError::BadVersion(ver_cmd));
    }
    let family = buf[13];
    let addr_len = match family {
        FAM_UNSPEC => 0,
        FAM_TCP4 => 12,
        FAM_TCP6 => 36,
        FAM_UNIX_STREAM => 216,
        other => return Err(ParseError::BadFamily(other)),
    };
    let body = &buf[FIXED_LEN..total];
    if body.len() < addr_len {
        return Err(ParseError::BadLength);
    }
    let a = &body[..addr_len];
    let port = |i: usize| u16::from_be_bytes([a[i], a[i + 1]]);
    let addresses = if ver_cmd == VER_CMD_LOCAL {
        Addresses::Unspec
    } else {
        match family {
            FAM_TCP4 => Addresses::V4 {
                src: SocketAddrV4::new(Ipv4Addr::new(a[0], a[1], a[2], a[3]), port(8)),
                dst: SocketAddrV4::new(Ipv4Addr::new(a[4], a[5], a[6], a[7]), port(10)),
            },
            FAM_TCP6 => {
                let mut s = [0u8; 16];
                let mut d = [0u8; 16];
                s.copy_from_slice(&a[0..16]);
                d.copy_from_slice(&a[16..32]);
                Addresses::V6 {
                    src: SocketAddrV6::new(Ipv6Addr::from(s), port(32), 0, 0),
                    dst: SocketAddrV6::new(Ipv6Addr::from(d), port(34), 0, 0),
                }
            }
            _ => Addresses::Unspec,
        }
    };
    let mut tlvs = Vec::new();
    let mut rest = &body[addr_len..];
    while !rest.is_empty() {
        if rest.len() < 3 {
            return Err(ParseError::BadTlv);
        }
        let len = u16::from_be_bytes([rest[1], rest[2]]) as usize;
        if rest.len() < 3 + len {
            return Err(ParseError::BadTlv);
        }
        tlvs.push((rest[0], rest[3..3 + len].to_vec()));
        rest = &rest[3 + len..];
    }
    Ok((Header { addresses, tlvs }, total))
}

#[cfg(test)]
mod tests {
    use super::*;

    const NODE: &str = "ae58ff8833241ac82d6ff7611046ed67b5072d142c588d0063e942d9a75502b6";

    fn sa(s: &str) -> SocketAddr {
        s.parse().unwrap()
    }

    #[test]
    fn ipv4_exact_bytes() {
        let h = Header::for_peer(Some(sa("203.0.113.7:51820")), Some(sa("[::]:7777")), "ab", "direct-v4");
        let mut want = SIGNATURE.to_vec();
        want.extend_from_slice(&[0x21, 0x11, 0x00, 12 + 5 + 12]);
        want.extend_from_slice(&[203, 0, 113, 7, 0, 0, 0, 0]);
        want.extend_from_slice(&[0xCA, 0x6C, 0x1E, 0x61]);
        want.extend_from_slice(&[0xE0, 0x00, 0x02, b'a', b'b']);
        want.extend_from_slice(&[0xE1, 0x00, 0x09]);
        want.extend_from_slice(b"direct-v4");
        assert_eq!(h.encode(), want);
    }

    #[test]
    fn ipv6_layout() {
        let h = Header::for_peer(Some(sa("[2001:db8::42]:40000")), Some(sa("[::]:7777")), NODE, "direct-v6");
        let bytes = h.encode();
        assert_eq!(bytes[13], FAM_TCP6);
        let len = u16::from_be_bytes([bytes[14], bytes[15]]) as usize;
        assert_eq!(len, 36 + 3 + 64 + 3 + 9);
        assert_eq!(bytes.len(), FIXED_LEN + len);
        assert_eq!(&bytes[16..32], &"2001:db8::42".parse::<Ipv6Addr>().unwrap().octets());
        assert_eq!(&bytes[48..50], &40000u16.to_be_bytes());
        assert_eq!(&bytes[50..52], &7777u16.to_be_bytes());
    }

    #[test]
    fn relay_is_unspec() {
        let h = Header::for_peer(None, Some(sa("0.0.0.0:7777")), NODE, "relay");
        let bytes = h.encode();
        assert_eq!(bytes[12], VER_CMD_PROXY);
        assert_eq!(bytes[13], FAM_UNSPEC);
        assert_eq!(u16::from_be_bytes([bytes[14], bytes[15]]) as usize, 3 + 64 + 3 + 5);
        assert_eq!(bytes[16], TLV_NODE_ID);
    }

    #[test]
    fn mapped_peer_becomes_ipv4() {
        let h = Header::for_peer(Some(sa("[::ffff:198.51.100.3]:1234")), None, NODE, "direct-v4");
        assert_eq!(h.encode()[13], FAM_TCP4);
        assert_eq!(h.source(), Some(sa("198.51.100.3:1234")));
    }

    #[test]
    fn roundtrip() {
        for peer in [None, Some(sa("192.0.2.1:9")), Some(sa("[2001:db8::1]:65535"))] {
            let h = Header::for_peer(peer, Some(sa("[::]:1")), NODE, "mixed");
            let mut bytes = h.encode();
            let used = bytes.len();
            bytes.extend_from_slice(b"GET / HTTP/1.1\r\n");
            let (parsed, n) = parse(&bytes).unwrap();
            assert_eq!(n, used);
            assert_eq!(parsed, h);
            assert_eq!(parsed.node_id(), Some(NODE));
            assert_eq!(parsed.path(), Some("mixed"));
        }
    }

    #[test]
    fn secret_roundtrip() {
        let h = Header::for_peer(None, None, &"a".repeat(64), "relay").with_secret(Some("s3cret"));
        let bytes = h.encode();
        let (back, used) = parse(&bytes).unwrap();
        assert_eq!(used, bytes.len());
        assert_eq!(back.secret(), Some("s3cret"));
        assert_eq!(back.path(), Some("relay"));
        let none = Header::for_peer(None, None, &"a".repeat(64), "relay").with_secret(Some(""));
        assert_eq!(none.secret(), None);
    }

    #[test]
    fn parse_errors() {
        let bytes = Header::for_peer(None, None, NODE, "relay").encode();
        assert_eq!(parse(&bytes[..10]), Err(ParseError::Incomplete { needed: FIXED_LEN }));
        assert_eq!(parse(&bytes[..20]), Err(ParseError::Incomplete { needed: bytes.len() }));
        let mut bad = bytes.clone();
        bad[0] = b'G';
        assert_eq!(parse(&bad), Err(ParseError::BadSignature));
        let mut bad = bytes.clone();
        bad[12] = 0x11;
        assert_eq!(parse(&bad), Err(ParseError::BadVersion(0x11)));
        let mut bad = bytes.clone();
        bad[13] = 0x22;
        assert_eq!(parse(&bad), Err(ParseError::BadFamily(0x22)));
        let mut bad = bytes;
        bad[18] = 0xff;
        assert_eq!(parse(&bad), Err(ParseError::BadTlv));
    }
}
