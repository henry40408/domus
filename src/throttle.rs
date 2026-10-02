//! In-memory login throttling: exponential back-off per username, never a permanent lockout.

use std::collections::HashMap;
use std::sync::Mutex;

/// Failures allowed before the first lock.
const FREE_ATTEMPTS: u32 = 5;
const BASE_LOCK_SECS: i64 = 60;
const MAX_LOCK_SECS: i64 = 900;
/// Past this many tracked names, idle entries are dropped on the next failure.
const MAX_ENTRIES: usize = 10_000;

struct Entry {
    failures: u32,
    locked_until: i64,
    last_failure: i64,
}

#[derive(Default)]
pub struct LoginGuard {
    entries: Mutex<HashMap<String, Entry>>,
}

impl LoginGuard {
    /// Seconds to wait before `key` may try again, or `None` if it may try now.
    pub fn check(&self, key: &str, now: i64) -> Option<i64> {
        let entries = self.entries.lock().unwrap();
        let wait = entries.get(key)?.locked_until - now;
        (wait > 0).then_some(wait)
    }

    /// Records a failed attempt; from the fifth in a row the lock doubles each time (1 to 15 minutes).
    pub fn fail(&self, key: &str, now: i64) {
        let mut entries = self.entries.lock().unwrap();
        if entries.len() >= MAX_ENTRIES {
            entries.retain(|_, e| e.locked_until > now || now - e.last_failure < MAX_LOCK_SECS);
        }
        let e = entries.entry(key.to_string()).or_insert(Entry {
            failures: 0,
            locked_until: 0,
            last_failure: now,
        });
        if now - e.last_failure > MAX_LOCK_SECS {
            e.failures = 0;
        }
        e.failures += 1;
        e.last_failure = now;
        if e.failures >= FREE_ATTEMPTS {
            let doublings = (e.failures - FREE_ATTEMPTS).min(4);
            e.locked_until = now + (BASE_LOCK_SECS << doublings).min(MAX_LOCK_SECS);
        }
    }

    pub fn succeed(&self, key: &str) {
        self.entries.lock().unwrap().remove(key);
    }
}

/// Normalised throttle key: case-insensitive, bounded so junk names cannot bloat the map.
pub fn key(scope: &str, username: &str) -> String {
    let name: String = username.trim().chars().take(64).collect();
    format!("{scope}:{}", name.to_lowercase())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn locks_after_five_failures_and_backs_off() {
        let g = LoginGuard::default();
        for _ in 0..4 {
            g.fail("a", 1000);
            assert_eq!(g.check("a", 1000), None);
        }
        g.fail("a", 1000);
        assert_eq!(g.check("a", 1000), Some(60));
        assert_eq!(g.check("a", 1059), Some(1));
        assert_eq!(g.check("a", 1060), None);

        // the next failure doubles it
        g.fail("a", 1060);
        assert_eq!(g.check("a", 1060), Some(120));
        // and it is capped at 15 minutes
        let mut t = 1060;
        for _ in 0..10 {
            t += 100;
            g.fail("a", t);
        }
        assert_eq!(g.check("a", t), Some(900));
        // other names are unaffected
        assert_eq!(g.check("b", t), None);
    }

    #[test]
    fn success_resets_and_idle_failures_expire() {
        let g = LoginGuard::default();
        for _ in 0..5 {
            g.fail("a", 0);
        }
        assert!(g.check("a", 0).is_some());
        g.succeed("a");
        assert_eq!(g.check("a", 0), None);

        for _ in 0..4 {
            g.fail("b", 0);
        }
        // long idle: the count starts over, so one more failure does not lock
        g.fail("b", 10_000);
        assert_eq!(g.check("b", 10_000), None);
    }

    #[test]
    fn prunes_idle_entries_when_full() {
        let g = LoginGuard::default();
        for i in 0..MAX_ENTRIES {
            g.fail(&format!("n{i}"), 0);
        }
        g.fail("fresh", 10_000);
        assert!(g.entries.lock().unwrap().len() < MAX_ENTRIES);
    }

    #[test]
    fn key_is_case_insensitive_and_bounded() {
        assert_eq!(key("login", " Admin "), "login:admin");
        assert_eq!(key("login", &"x".repeat(500)).len(), "login:".len() + 64);
    }
}
