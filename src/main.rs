use domus::core::Core;
use domus::env::Env;
use domus::hue::HueManager;
use domus::store::Store;
use domus::{AppState, build_app, resume_hue};
use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
use std::sync::Arc;
use tracing::info;

fn prepare_data_dir(dir: &std::path::Path) -> std::io::Result<()> {
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)?;
    std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))
}

#[tokio::main]
async fn main() {
    let env = match Env::from_env() {
        Ok(e) => e,
        Err(e) => {
            eprintln!("domus: {e}");
            std::process::exit(2);
        }
    };
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::new(&env.log))
        .init();

    if let Err(e) = prepare_data_dir(&env.data_dir) {
        eprintln!(
            "domus: cannot prepare data dir {}: {e}",
            env.data_dir.display()
        );
        std::process::exit(1);
    }
    let db_path = env.data_dir.join("domus.db");
    let store = match Store::open(&db_path) {
        Ok(s) => Arc::new(s),
        Err(e) => {
            eprintln!("domus: cannot open {}: {e}", db_path.display());
            std::process::exit(1);
        }
    };
    let _ = std::fs::set_permissions(&db_path, std::fs::Permissions::from_mode(0o600));

    let core = Core::new(store);
    let hue = HueManager::new(core.clone());
    resume_hue(&core, &hue);
    let app = build_app(AppState { core, hue });

    let listener = match tokio::net::TcpListener::bind(env.bind).await {
        Ok(l) => l,
        Err(e) => {
            eprintln!("domus: cannot bind {}: {e}", env.bind);
            std::process::exit(1);
        }
    };
    info!("listening on http://{}", env.bind);
    let shutdown = async {
        let mut term = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
            .expect("install SIGTERM handler");
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {}
            _ = term.recv() => {}
        }
        info!("shutting down");
    };
    if let Err(e) = axum::serve(listener, app)
        .with_graceful_shutdown(shutdown)
        .await
    {
        eprintln!("domus: server error: {e}");
        std::process::exit(1);
    }
}
