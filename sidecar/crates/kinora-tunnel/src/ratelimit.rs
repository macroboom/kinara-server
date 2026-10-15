//! Sliding window limiter for new connections per peer.

use std::collections::{HashMap, VecDeque};
use std::hash::Hash;
use std::time::{Duration, Instant};

#[derive(Debug)]
pub struct RateWindow<K> {
    max: u32,
    period: Duration,
    hits: HashMap<K, VecDeque<Instant>>,
}

impl<K: Hash + Eq + Clone> RateWindow<K> {
    /// `max` of zero disables the limit.
    pub fn new(max: u32, period: Duration) -> Self {
        Self { max, period, hits: HashMap::new() }
    }

    /// Records an attempt and says whether it is allowed. Refused attempts
    /// are not recorded, so a client that backs off recovers on schedule.
    pub fn allow(&mut self, key: &K, now: Instant) -> bool {
        if self.max == 0 {
            return true;
        }
        let period = self.period;
        let queue = self.hits.entry(key.clone()).or_default();
        while let Some(front) = queue.front() {
            if now.saturating_duration_since(*front) >= period {
                queue.pop_front();
            } else {
                break;
            }
        }
        if queue.len() >= self.max as usize {
            return false;
        }
        queue.push_back(now);
        // Keep the map from growing with peers that went away.
        if self.hits.len() > 1024 {
            self.hits.retain(|_, q| q.back().is_some_and(|t| now.saturating_duration_since(*t) < period));
        }
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn limits_and_recovers() {
        let start = Instant::now();
        let mut w = RateWindow::new(3, Duration::from_secs(60));
        assert!(w.allow(&"a", start));
        assert!(w.allow(&"a", start + Duration::from_secs(1)));
        assert!(w.allow(&"a", start + Duration::from_secs(2)));
        assert!(!w.allow(&"a", start + Duration::from_secs(3)));
        assert!(w.allow(&"b", start + Duration::from_secs(3)));
        assert!(w.allow(&"a", start + Duration::from_secs(60)));
        assert!(!w.allow(&"a", start + Duration::from_secs(60)));
        assert!(w.allow(&"a", start + Duration::from_secs(62)));
    }

    #[test]
    fn zero_disables() {
        let mut w = RateWindow::new(0, Duration::from_secs(60));
        let now = Instant::now();
        for _ in 0..1000 {
            assert!(w.allow(&1u8, now));
        }
    }
}
