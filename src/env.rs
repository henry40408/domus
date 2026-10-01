//! Startup configuration, read from environment variables only.

use std::net::SocketAddr;
use std::path::PathBuf;

#[derive(Debug, Clone, PartialEq)]
pub struct Env {
    pub bind: SocketAddr,
    pub data_dir: PathBuf,
    pub log: String,
}

impl Env {
    pub fn from_lookup(get: impl Fn(&str) -> Option<String>) -> Result<Self, String> {
        let bind = get("DOMUS_BIND").unwrap_or_else(|| "127.0.0.1:8123".into());
        let bind = bind
            .parse()
            .map_err(|e| format!("DOMUS_BIND={bind:?} is not a valid socket address: {e}"))?;
        Ok(Self {
            bind,
            data_dir: get("DOMUS_DATA_DIR")
                .unwrap_or_else(|| "./data".into())
                .into(),
            log: get("DOMUS_LOG").unwrap_or_else(|| "info".into()),
        })
    }

    pub fn from_env() -> Result<Self, String> {
        Self::from_lookup(|k| std::env::var(k).ok())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn defaults_and_overrides() {
        let e = Env::from_lookup(|_| None).unwrap();
        assert_eq!(e.bind, "127.0.0.1:8123".parse().unwrap());
        assert_eq!(e.data_dir, PathBuf::from("./data"));
        let e =
            Env::from_lookup(|k| (k == "DOMUS_BIND").then(|| "0.0.0.0:9000".to_string())).unwrap();
        assert_eq!(e.bind.port(), 9000);
        assert!(Env::from_lookup(|k| (k == "DOMUS_BIND").then(|| "nope".to_string())).is_err());
    }
}
