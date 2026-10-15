//! Byte size parsing for window flags, so "8M" works as well as "8388608".

/// Parses a byte count with an optional binary suffix: K, M or G, optionally
/// followed by "iB" or "B". Case does not matter.
pub fn parse_size(input: &str) -> Result<u64, String> {
    let s = input.trim();
    let split = s.find(|c: char| !c.is_ascii_digit()).unwrap_or(s.len());
    let (digits, suffix) = s.split_at(split);
    if digits.is_empty() {
        return Err(format!("invalid size {input:?}: expected a number like 4194304 or 4M"));
    }
    let n: u64 = digits.parse().map_err(|_| format!("invalid size {input:?}"))?;
    let mult: u64 = match suffix.trim().to_ascii_lowercase().as_str() {
        "" | "b" => 1,
        "k" | "kb" | "kib" => 1 << 10,
        "m" | "mb" | "mib" => 1 << 20,
        "g" | "gb" | "gib" => 1 << 30,
        other => return Err(format!("invalid size suffix {other:?} in {input:?}")),
    };
    n.checked_mul(mult).ok_or_else(|| format!("size {input:?} is too large"))
}

/// Same as `parse_size` but limited to what QUIC flow control windows accept
/// through `VarInt::from_u32`.
pub fn parse_window(input: &str) -> Result<u32, String> {
    let n = parse_size(input)?;
    if n < 64 * 1024 {
        return Err(format!("window {input:?} is below the 64K minimum"));
    }
    u32::try_from(n).map_err(|_| format!("window {input:?} must be below 4G"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sizes() {
        assert_eq!(parse_size("4194304"), Ok(4 << 20));
        assert_eq!(parse_size("4M"), Ok(4 << 20));
        assert_eq!(parse_size("4mib"), Ok(4 << 20));
        assert_eq!(parse_size("512K"), Ok(512 << 10));
        assert_eq!(parse_size("1G"), Ok(1 << 30));
        assert_eq!(parse_size(" 16 MB "), Ok(16 << 20));
        assert!(parse_size("M").is_err());
        assert!(parse_size("4X").is_err());
        assert!(parse_size("-4").is_err());
    }

    #[test]
    fn windows() {
        assert_eq!(parse_window("16M"), Ok(16 << 20));
        assert!(parse_window("1K").is_err());
        assert!(parse_window("4G").is_err());
    }
}
