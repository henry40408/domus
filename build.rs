use std::path::Path;
use std::process::Command;

fn main() {
    println!("cargo:rerun-if-env-changed=GIT_VERSION");
    // Only watch paths that exist: a missing path makes cargo rerun this script on every build
    // (in a git worktree `.git` is a file, so `.git/HEAD` is absent).
    for path in [".git/HEAD", ".git/index"] {
        if Path::new(path).exists() {
            println!("cargo:rerun-if-changed={path}");
        }
    }
    println!("cargo:rustc-env=GIT_VERSION={}", git_version());
}

fn git_version() -> String {
    // Docker builds have no .git directory, so CI passes the version in.
    if let Ok(version) = std::env::var("GIT_VERSION")
        && !version.is_empty()
        && version != "dev"
    {
        return version;
    }
    // Tag if HEAD is tagged, else tag-commits-hash; `-dirty` marks uncommitted changes.
    Command::new("git")
        .args(["describe", "--tags", "--always", "--dirty"])
        .output()
        .ok()
        .filter(|o| o.status.success())
        .map_or_else(
            || "dev".to_string(),
            |o| String::from_utf8_lossy(&o.stdout).trim().to_string(),
        )
}
