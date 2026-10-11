//! mDNS (Bonjour) advertisement so the Home Assistant Companion apps list domus as a server to
//! pick instead of making the user type its URL.

use crate::mobile::HA_VERSION;
use crate::util::random_hex;
use mdns_sd::{ServiceDaemon, ServiceInfo};
use std::net::{IpAddr, SocketAddr, UdpSocket};
use std::path::Path;
use std::time::Duration;
use tracing::{info, warn};

const SERVICE_TYPE: &str = "_home-assistant._tcp.local.";
const INSTANCE_FILE: &str = "instance-id";

/// The address to put in the advertised URL: the bind address itself, or this host's LAN address
/// when it listens on all interfaces. `None` for loopback, which no other device can reach.
pub fn advertise_address(bind: SocketAddr) -> Option<IpAddr> {
    match bind.ip() {
        ip if ip.is_loopback() => None,
        ip if ip.is_unspecified() => local_ip(),
        // Without a zone ID a link-local IPv6 address is of no use to the phone.
        IpAddr::V6(ip) if ip.is_unicast_link_local() => None,
        ip => Some(ip),
    }
}

/// The address of the interface that would reach the LAN. Connecting a UDP socket sends nothing;
/// it only makes the OS pick a route.
fn local_ip() -> Option<IpAddr> {
    let socket = UdpSocket::bind("0.0.0.0:0").ok()?;
    socket.connect("192.0.2.1:9").ok()?;
    let ip = socket.local_addr().ok()?.ip();
    (!ip.is_loopback() && !ip.is_unspecified()).then_some(ip)
}

/// A UUID that stays the same across restarts, so the apps recognise the server as one they know.
fn instance_id(data_dir: &Path) -> String {
    let path = data_dir.join(INSTANCE_FILE);
    if let Ok(saved) = std::fs::read_to_string(&path) {
        let saved = saved.trim();
        if is_uuid(saved) {
            return saved.to_string();
        }
    }
    let h = random_hex(16);
    let id = format!(
        "{}-{}-{}-{}-{}",
        &h[..8],
        &h[8..12],
        &h[12..16],
        &h[16..20],
        &h[20..]
    );
    // Write then rename, so a crash cannot leave a half-written id behind. Without a saved id the
    // apps see a new server after every restart.
    let tmp = data_dir.join(format!("{INSTANCE_FILE}.tmp"));
    if let Err(e) = std::fs::write(&tmp, &id).and_then(|()| std::fs::rename(&tmp, &path)) {
        warn!(
            "cannot save {}: {e}; apps will see a new server after a restart",
            path.display()
        );
    }
    id
}

fn is_uuid(s: &str) -> bool {
    s.len() == 36
        && s.char_indices().all(|(i, c)| match i {
            8 | 13 | 18 | 23 => c == '-',
            _ => c.is_ascii_hexdigit(),
        })
}

/// The TXT records Home Assistant publishes; the apps read the URL and the name from them.
fn properties(ip: IpAddr, port: u16, uuid: &str) -> Vec<(String, String)> {
    let url = match ip {
        IpAddr::V4(ip) => format!("http://{ip}:{port}"),
        IpAddr::V6(ip) => format!("http://[{ip}]:{port}"),
    };
    [
        ("base_url", url.as_str()),
        ("internal_url", url.as_str()),
        ("external_url", ""),
        ("location_name", "domus"),
        ("requires_api_password", "true"),
        ("uuid", uuid),
        ("version", HA_VERSION),
    ]
    .into_iter()
    .map(|(k, v)| (k.to_string(), v.to_string()))
    .collect()
}

/// Starts advertising; the returned daemon keeps doing so until it is shut down. Failures only
/// cost discovery, so they are logged and the server carries on.
pub fn start(bind: SocketAddr, data_dir: &Path) -> Option<ServiceDaemon> {
    let Some(ip) = advertise_address(bind) else {
        info!("not advertising over mDNS: {bind} is not an address other devices can reach");
        return None;
    };
    let uuid = instance_id(data_dir);
    // The short id keeps two domus instances on one LAN from replacing each other.
    let name = format!("domus-{}", &uuid[..8]);
    let port = bind.port();
    let daemon = register(&name, ip, port, &uuid)
        .inspect_err(|e| warn!("mDNS discovery disabled: {e}"))
        .ok()?;
    info!("advertising on the LAN as {SERVICE_TYPE} at {ip}:{port}");
    Some(daemon)
}

fn register(name: &str, ip: IpAddr, port: u16, uuid: &str) -> mdns_sd::Result<ServiceDaemon> {
    let host = format!("{name}.local.");
    let props = properties(ip, port, uuid);
    let info = ServiceInfo::new(SERVICE_TYPE, name, &host, ip, port, props.as_slice())?;
    let daemon = ServiceDaemon::new()?;
    if let Err(e) = daemon.register(info) {
        let _ = daemon.shutdown();
        return Err(e);
    }
    Ok(daemon)
}

/// Stops advertising. The goodbye packets go out on the daemon's own thread, so wait a moment for
/// them: phones then drop the entry instead of listing a stopped server until its TTL runs out.
pub fn stop(daemon: &ServiceDaemon) {
    if let Ok(done) = daemon.shutdown() {
        let _ = done.recv_timeout(Duration::from_secs(1));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn loopback_is_never_advertised() {
        assert_eq!(advertise_address("127.0.0.1:8123".parse().unwrap()), None);
        assert_eq!(advertise_address("[::1]:8123".parse().unwrap()), None);
        assert_eq!(advertise_address("[fe80::1]:8123".parse().unwrap()), None);
        let dir = std::env::temp_dir();
        assert!(start("127.0.0.1:8123".parse().unwrap(), &dir).is_none());
    }

    #[test]
    fn a_specific_bind_address_is_used_as_is() {
        let ip = advertise_address("192.168.1.20:8123".parse().unwrap());
        assert_eq!(ip, Some("192.168.1.20".parse().unwrap()));
    }

    #[test]
    fn the_unspecified_address_resolves_to_a_lan_address_or_nothing() {
        if let Some(ip) = advertise_address("0.0.0.0:8123".parse().unwrap()) {
            assert!(!ip.is_loopback() && !ip.is_unspecified());
        }
    }

    #[test]
    fn instance_id_is_a_stable_uuid() {
        let dir = std::env::temp_dir().join(format!("domus-mdns-{}", random_hex(8)));
        std::fs::create_dir_all(&dir).unwrap();
        let first = instance_id(&dir);
        assert_eq!(first.len(), 36);
        assert_eq!(first.matches('-').count(), 4);
        assert_eq!(instance_id(&dir), first);
        for bad in [
            "  \n",
            "abc",
            "ключ-ключ-ключ",
            "not.a.uuid.at.all.but.36.chars.long",
        ] {
            std::fs::write(dir.join(INSTANCE_FILE), bad).unwrap();
            let id = instance_id(&dir);
            assert!(is_uuid(&id), "{bad:?} -> {id}");
            assert_ne!(id, first);
        }
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn txt_records_carry_the_url_and_version() {
        let p = properties("192.168.1.20".parse().unwrap(), 8123, "u-1");
        let get = |k: &str| p.iter().find(|(n, _)| n == k).map(|(_, v)| v.as_str());
        assert_eq!(get("base_url"), Some("http://192.168.1.20:8123"));
        assert_eq!(get("internal_url"), Some("http://192.168.1.20:8123"));
        assert_eq!(get("uuid"), Some("u-1"));
        assert_eq!(get("version"), Some(HA_VERSION));
        let p = properties("fe80::1".parse().unwrap(), 80, "u");
        assert!(
            p.iter()
                .any(|(k, v)| k == "base_url" && v == "http://[fe80::1]:80")
        );
    }

    #[test]
    fn registering_and_stopping_does_not_panic() {
        // Sandboxes may have no multicast; the daemon is optional, so either outcome is fine.
        if let Ok(daemon) = register("domus-test", "192.168.1.20".parse().unwrap(), 8123, "u") {
            stop(&daemon);
        }
    }

    #[test]
    fn invalid_names_are_rejected_instead_of_panicking() {
        let ip = "192.168.1.20".parse().unwrap();
        assert!(register("", ip, 8123, "u").is_err());
    }

    #[test]
    fn an_unwritable_data_dir_still_gives_an_id() {
        let dir = std::env::temp_dir().join(format!("domus-missing-{}", random_hex(8)));
        assert!(is_uuid(&instance_id(&dir)));
        if let Some(daemon) = start("0.0.0.0:8123".parse().unwrap(), &dir) {
            stop(&daemon);
        }
    }
}
