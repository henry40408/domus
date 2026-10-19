//! A domus server under test. The binary is spawned directly, not through
//! `cargo run`, so killing our PID kills the server rather than leaving it
//! holding the port.

use std::io::{BufRead, BufReader};
use std::net::TcpListener;
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStderr, Command, Stdio};
use std::time::{Duration, Instant};

use anyhow::{Context, Result, bail};

const STARTUP_TIMEOUT: Duration = Duration::from_secs(30);

pub struct Server {
    pub base_url: String,
    pub setup_code: String,
    child: Child,
    // Held for its Drop: removes the database directory.
    _data: tempfile::TempDir,
}

impl Server {
    pub async fn start() -> Result<Self> {
        let binary = ensure_binary()?;
        let data = tempfile::Builder::new().prefix("domus-e2e-").tempdir()?;
        let port = TcpListener::bind("127.0.0.1:0")
            .context("probing for a free port")?
            .local_addr()?
            .port();
        let base_url = format!("http://127.0.0.1:{port}");
        let mut child = Command::new(&binary)
            .current_dir(repo_root())
            .env("DOMUS_BIND", format!("127.0.0.1:{port}"))
            .env("DOMUS_DATA_DIR", data.path())
            .env("DOMUS_MDNS", "0")
            .env("DOMUS_LOG", "warn")
            .stdout(Stdio::inherit())
            .stderr(Stdio::piped())
            .spawn()
            .with_context(|| format!("spawning {}", binary.display()))?;
        let stderr = child.stderr.take().context("capturing domus stderr")?;
        let mut server = Self {
            base_url,
            setup_code: String::new(),
            child,
            _data: data,
        };
        // After `server` exists, an early error drops it and kills the child.
        server.setup_code = read_setup_code(stderr)?;
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

/// The server prints a random setup code on stderr at startup. Reads up to that line, then
/// keeps forwarding the rest so the server never blocks on a full pipe.
fn read_setup_code(stderr: ChildStderr) -> Result<String> {
    let mut lines = BufReader::new(stderr).lines();
    let code = loop {
        let line = lines
            .next()
            .context("domus exited before printing a setup code")??;
        eprintln!("{line}");
        if let Some((_, code)) = line.split_once("Setup code: ") {
            break code.trim().to_string();
        }
    };
    std::thread::spawn(move || lines.map_while(Result::ok).for_each(|l| eprintln!("{l}")));
    Ok(code)
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
