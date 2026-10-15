//! Deterministic file content shared by the test server and the e2e checker,
//! so a 2 GB "file" needs no disk and any byte range can be verified.

/// SplitMix64: fast, and every 8 byte block depends only on its index.
fn mix(mut z: u64) -> u64 {
    z = z.wrapping_add(0x9E37_79B9_7F4A_7C15);
    z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
    z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
    z ^ (z >> 31)
}

/// Fills `buf` with the bytes of the virtual file starting at `offset`.
pub fn fill(seed: u64, offset: u64, buf: &mut [u8]) {
    let mut pos = offset;
    let mut i = 0;
    while i < buf.len() {
        let block = pos / 8;
        let within = (pos % 8) as usize;
        let bytes = mix(seed ^ block).to_le_bytes();
        let take = (8 - within).min(buf.len() - i);
        buf[i..i + take].copy_from_slice(&bytes[within..within + take]);
        i += take;
        pos += take as u64;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ranges_agree_with_whole() {
        let mut whole = vec![0u8; 1000];
        fill(7, 0, &mut whole);
        for (start, len) in [(0usize, 1usize), (3, 17), (8, 8), (13, 500), (999, 1)] {
            let mut part = vec![0u8; len];
            fill(7, start as u64, &mut part);
            assert_eq!(part, whole[start..start + len]);
        }
    }

    #[test]
    fn seed_changes_content() {
        let mut a = [0u8; 32];
        let mut b = [0u8; 32];
        fill(1, 0, &mut a);
        fill(2, 0, &mut b);
        assert_ne!(a, b);
    }
}
