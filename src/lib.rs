//! domus: a lightweight Home Assistant compatible server (REST subset) for Hue lights.

pub mod admin;
pub mod api;
pub mod core;
pub mod env;
pub mod hue;
pub mod store;
pub mod util;

use axum::Router;
use std::sync::Arc;

pub use api::AppState;

/// Builds the full HTTP application: HA REST API, admin API and the embedded admin page.
pub fn build_app(app: AppState) -> Router {
    Router::new()
        .merge(api::router(app.clone()))
        .nest("/api/domus", admin::router(app))
        .fallback(admin::static_files)
}

/// Starts the Hue integration if a bridge was paired earlier.
pub fn resume_hue(core: &Arc<core::Core>, hue: &hue::HueManager) {
    if let Some(b) = core.store().hue_get() {
        hue.start(&hue::bridge_base(&b.ip), &b.key);
    }
}
