//! Startup configuration, read from environment variables only.

use std::net::SocketAddr;
use std::path::PathBuf;

#[derive(Debug, Clone, PartialEq)]
pub struct Env {
    pub bind: SocketAddr,
    pub data_dir: PathBuf,
    pub log: String,
    /// Fixed first-run setup code; a random one is generated when unset.
    pub setup_code: Option<String>,
    /// Whether to advertise on the LAN over mDNS (only when bound to a non-loopback address).
    pub mdns: bool,
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
            setup_code: get("DOMUS_SETUP_CODE").filter(|c| !c.is_empty()),
            mdns: !matches!(
                get("DOMUS_MDNS")
                    .as_deref()
                    .map(str::to_ascii_lowercase)
                    .as_deref(),
                Some("0" | "false" | "off" | "no")
            ),
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
        assert_eq!(e.setup_code, None);
        let e = Env::from_lookup(|k| (k == "DOMUS_SETUP_CODE").then(|| "abc".to_string())).unwrap();
        assert_eq!(e.setup_code.as_deref(), Some("abc"));
        let e = Env::from_lookup(|k| (k == "DOMUS_SETUP_CODE").then(String::new)).unwrap();
        assert_eq!(e.setup_code, None);
        let e =
            Env::from_lookup(|k| (k == "DOMUS_BIND").then(|| "0.0.0.0:9000".to_string())).unwrap();
        assert_eq!(e.bind.port(), 9000);
        assert!(Env::from_lookup(|k| (k == "DOMUS_BIND").then(|| "nope".to_string())).is_err());
        assert!(e.mdns);
        for off in ["0", "false", "OFF", "no"] {
            let e = Env::from_lookup(|k| (k == "DOMUS_MDNS").then(|| off.to_string())).unwrap();
            assert!(!e.mdns, "{off}");
        }
        let e = Env::from_lookup(|k| (k == "DOMUS_MDNS").then(|| "1".to_string())).unwrap();
        assert!(e.mdns);
    }
}
