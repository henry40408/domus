//! A domus server under test. The binary is spawned directly, not through
//! `cargo run`, so killing our PID kills the server rather than leaving it
//! holding the port.

use std::net::TcpListener;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

use anyhow::{Context, Result, bail};

const STARTUP_TIMEOUT: Duration = Duration::from_secs(30);

pub struct Server {
    pub base_url: String,
    child: Child,
    // Held for its Drop: removes the database directory.
    _data: tempfile::TempDir,
}

impl Server {
    pub async fn start(setup_code: &str) -> Result<Self> {
        let binary = ensure_binary()?;
        let data = tempfile::Builder::new().prefix("domus-e2e-").tempdir()?;
        let port = TcpListener::bind("127.0.0.1:0")
            .context("probing for a free port")?
            .local_addr()?
            .port();
        let base_url = format!("http://127.0.0.1:{port}");
        let child = Command::new(&binary)
            .current_dir(repo_root())
            .env("DOMUS_BIND", format!("127.0.0.1:{port}"))
            .env("DOMUS_DATA_DIR", data.path())
            .env("DOMUS_SETUP_CODE", setup_code)
            .env("DOMUS_MDNS", "0")
            .env("DOMUS_LOG", "warn")
            .stdout(Stdio::inherit())
            .stderr(Stdio::inherit())
            .spawn()
            .with_context(|| format!("spawning {}", binary.display()))?;
        let mut server = Self {
            base_url,
            child,
            _data: data,
        };
        server.wait_until_healthy().await?;
        Ok(server)
    }

    async fn wait_until_healthy(&mut self) -> Result<()> {
        let client = reqwest::Client::new();
        let health = format!("{}/health", self.base_url);
        let deadline = Instant::now() + STARTUP_TIMEOUT;
        while Instant::now() < deadline {
            if let Some(status) = self.child.try_wait()? {
                bail!("domus exited during startup: {status}");
            }
            if let Ok(r) = client.get(&health).send().await
                && r.status().is_success()
            {
                return Ok(());
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        bail!("domus did not answer {health} within {STARTUP_TIMEOUT:?}")
    }
}

impl Drop for Server {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

/// The dev-profile binary. Never built here: the web assets are embedded at
/// compile time, so a stale binary would screenshot stale UI.
fn ensure_binary() -> Result<PathBuf> {
    let binary = repo_root().join("target/debug/domus");
    if !binary.is_file() {
        bail!(
            "{} is missing; run `cargo build` in the repository root first",
            binary.display()
        );
    }
    Ok(binary)
}

/// The repository root: the parent of this crate's directory.
pub fn repo_root() -> &'static Path {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .expect("e2e/ has a parent")
}
