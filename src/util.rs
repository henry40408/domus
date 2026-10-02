//! Small helpers: randomness, hashing, password storage, timestamps.

use argon2::Argon2;
use sha2::{Digest, Sha256};
use std::sync::OnceLock;
use std::time::{SystemTime, UNIX_EPOCH};

pub fn hex(bytes: &[u8]) -> String {
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push_str(&format!("{b:02x}"));
    }
    s
}

pub fn random_bytes<const N: usize>() -> [u8; N] {
    let mut out = [0u8; N];
    for b in out.iter_mut() {
        *b = rand::random();
    }
    out
}

pub fn random_hex(bytes: usize) -> String {
    let mut s = String::with_capacity(bytes * 2);
    for _ in 0..bytes {
        let b: u8 = rand::random();
        s.push_str(&format!("{b:02x}"));
    }
    s
}

pub fn sha256_hex(input: &str) -> String {
    let digest = Sha256::digest(input.as_bytes());
    hex(digest.as_slice())
}

pub fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

fn derive(password: &str, salt: &[u8]) -> [u8; 32] {
    let mut out = [0u8; 32];
    Argon2::default()
        .hash_password_into(password.as_bytes(), salt, &mut out)
        .expect("argon2 parameters are valid");
    out
}

/// Returns `<salt hex>$<argon2id hash hex>`.
pub fn hash_password(password: &str) -> String {
    let salt = random_bytes::<16>();
    format!("{}${}", hex(&salt), hex(&derive(password, &salt)))
}

pub fn verify_password(password: &str, stored: &str) -> bool {
    let Some((salt_hex, hash_hex)) = stored.split_once('$') else {
        return false;
    };
    let Some(salt) = unhex(salt_hex) else {
        return false;
    };
    constant_time_eq(
        hex(&derive(password, &salt)).as_bytes(),
        hash_hex.as_bytes(),
    )
}

/// Burns the same argon2 time as a real check, for usernames that do not exist,
/// so response time does not reveal which usernames are valid.
pub fn verify_dummy(password: &str) {
    static DUMMY: OnceLock<String> = OnceLock::new();
    let stored = DUMMY.get_or_init(|| hash_password("domus dummy password"));
    verify_password(password, stored);
}

/// Computes the dummy hash up front so the first unknown-user login is not faster than later ones.
pub fn warm_dummy() {
    verify_dummy("");
}

fn unhex(s: &str) -> Option<Vec<u8>> {
    if !s.len().is_multiple_of(2) {
        return None;
    }
    (0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&s[i..i + 2], 16).ok())
        .collect()
}

pub fn now_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Home Assistant style timestamp, e.g. `2026-10-01T12:34:56.123456+00:00`.
pub fn rfc3339_micros(t: SystemTime) -> String {
    let d = t.duration_since(UNIX_EPOCH).unwrap_or_default();
    let secs = d.as_secs() as i64;
    let micros = d.subsec_micros();
    let (y, m, day) = civil_from_days(secs.div_euclid(86_400));
    let rem = secs.rem_euclid(86_400);
    format!(
        "{y:04}-{m:02}-{day:02}T{:02}:{:02}:{:02}.{micros:06}+00:00",
        rem / 3600,
        rem % 3600 / 60,
        rem % 60
    )
}

/// Days since 1970-01-01 to (year, month, day); Howard Hinnant's algorithm.
fn civil_from_days(days: i64) -> (i64, i64, i64) {
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    (yoe + era * 400 + i64::from(m <= 2), m, d)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    #[test]
    fn timestamp_format() {
        let t = UNIX_EPOCH + Duration::new(1_790_858_696, 123_456_000);
        assert_eq!(rfc3339_micros(t), "2026-10-01T12:44:56.123456+00:00");
        assert_eq!(
            rfc3339_micros(UNIX_EPOCH),
            "1970-01-01T00:00:00.000000+00:00"
        );
        // leap day
        let t = UNIX_EPOCH + Duration::from_secs(1_709_208_000);
        assert_eq!(rfc3339_micros(t), "2024-02-29T12:00:00.000000+00:00");
    }

    #[test]
    fn password_roundtrip() {
        let stored = hash_password("hunter2");
        assert!(verify_password("hunter2", &stored));
        assert!(!verify_password("hunter3", &stored));
        assert!(!verify_password("hunter2", "garbage"));
        assert_ne!(stored, hash_password("hunter2"), "salt must differ");
    }

    #[test]
    fn sha256_known_vector() {
        assert_eq!(
            sha256_hex("abc"),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
    }

    #[test]
    fn random_hex_length() {
        assert_eq!(random_hex(16).len(), 32);
        assert_ne!(random_hex(16), random_hex(16));
    }
}
