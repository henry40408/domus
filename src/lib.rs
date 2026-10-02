//! domus: a lightweight Home Assistant compatible server (REST subset) for Hue lights.

/// What this build is: the git tag (or `tag-N-hash`, or a bare hash) it was made from, set by
/// `build.rs`; `dev` when no git information was available.
pub const GIT_VERSION: &str = env!("GIT_VERSION");

pub mod admin;
pub mod api;
pub mod core;
pub mod env;
pub mod health;
pub mod hue;
pub mod store;
pub mod throttle;
pub mod util;

use axum::Router;
use axum::extract::Request;
use axum::http::{HeaderName, HeaderValue, header};
use axum::middleware::{self, Next};
use axum::response::Response;
use std::sync::Arc;

pub use api::AppState;

const CSP: &str = "default-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'";

/// Adds browser hardening headers to every response.
async fn security_headers(req: Request, next: Next) -> Response {
    let https = req
        .headers()
        .get("x-forwarded-proto")
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| v.eq_ignore_ascii_case("https"));
    // Neither the admin API nor a health verdict may be cached.
    let no_store = req.uri().path().starts_with("/api/domus") || req.uri().path() == "/health";
    let mut resp = next.run(req).await;
    let h = resp.headers_mut();
    let mut set = |name: HeaderName, value: &'static str| {
        h.insert(name, HeaderValue::from_static(value));
    };
    set(header::CONTENT_SECURITY_POLICY, CSP);
    set(header::X_CONTENT_TYPE_OPTIONS, "nosniff");
    set(header::X_FRAME_OPTIONS, "DENY");
    set(header::REFERRER_POLICY, "no-referrer");
    if no_store {
        set(header::CACHE_CONTROL, "no-store");
    }
    if https {
        set(
            header::STRICT_TRANSPORT_SECURITY,
            "max-age=31536000; includeSubDomains",
        );
    }
    resp
}

/// Builds the full HTTP application: HA REST API, admin API and the embedded admin page.
pub fn build_app(app: AppState) -> Router {
    let health = Router::new()
        .route("/health", axum::routing::get(health::health))
        .with_state(app.clone());
    Router::new()
        .merge(health)
        .merge(api::router(app.clone()))
        .nest("/api/domus", admin::router(app))
        .fallback(admin::static_files)
        .layer(middleware::from_fn(security_headers))
}

/// Starts the Hue integration if a bridge was paired earlier.
pub async fn resume_hue(core: &Arc<core::Core>, hue: &hue::HueManager) {
    if let Some(b) = core.store().hue_get().await {
        hue.start(&hue::bridge_base(&b.ip), &b.key);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::Body;
    use axum::http::Request as HttpRequest;
    use axum::http::StatusCode;
    use tower::ServiceExt;

    #[test]
    fn build_knows_its_version() {
        assert!(!GIT_VERSION.is_empty());
        assert!(!GIT_VERSION.contains(char::is_whitespace));
    }

    async fn app() -> Router {
        let core = core::Core::new(Arc::new(store::Store::open_memory().await.unwrap()));
        build_app(AppState::new(core.clone(), hue::HueManager::new(core)))
    }

    async fn get(r: &Router, uri: &str, proto: Option<&str>) -> Response {
        let mut b = HttpRequest::builder().uri(uri);
        if let Some(p) = proto {
            b = b.header("x-forwarded-proto", p);
        }
        r.clone()
            .oneshot(b.body(Body::empty()).unwrap())
            .await
            .unwrap()
    }

    #[tokio::test]
    async fn health_is_public_uncached_and_follows_the_database() {
        let core = core::Core::new(Arc::new(store::Store::open_memory().await.unwrap()));
        let store = core.store().clone();
        let r = build_app(AppState::new(core.clone(), hue::HueManager::new(core)));

        let resp = get(&r, "/health", None).await; // no cookie, no token
        assert_eq!(resp.status(), StatusCode::OK);
        assert_eq!(resp.headers()[header::CACHE_CONTROL], "no-store");
        let body = axum::body::to_bytes(resp.into_body(), usize::MAX)
            .await
            .unwrap();
        let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(
            v,
            serde_json::json!({"status": "ok", "version": GIT_VERSION})
        );

        store.close_for_test().await;
        let resp = get(&r, "/health", None).await;
        assert_eq!(resp.status(), StatusCode::SERVICE_UNAVAILABLE);
        let body = axum::body::to_bytes(resp.into_body(), usize::MAX)
            .await
            .unwrap();
        let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(v["status"], "error");
    }

    #[tokio::test]
    async fn security_headers_on_every_response() {
        let r = app().await;
        for uri in ["/", "/nope", "/api/domus/status", "/api/states/light.x"] {
            let h = get(&r, uri, None).await;
            let h = h.headers();
            assert!(
                h[header::CONTENT_SECURITY_POLICY]
                    .to_str()
                    .unwrap()
                    .contains("frame-ancestors 'none'"),
                "{uri}"
            );
            assert_eq!(h[header::X_CONTENT_TYPE_OPTIONS], "nosniff", "{uri}");
            assert_eq!(h[header::X_FRAME_OPTIONS], "DENY", "{uri}");
            assert_eq!(h[header::REFERRER_POLICY], "no-referrer", "{uri}");
            assert!(!h.contains_key(header::STRICT_TRANSPORT_SECURITY), "{uri}");
        }
    }

    #[tokio::test]
    async fn no_store_only_for_admin_api_and_hsts_only_behind_https() {
        let r = app().await;
        assert_eq!(
            get(&r, "/api/domus/status", None).await.headers()[header::CACHE_CONTROL],
            "no-store"
        );
        // the page is revalidated (see admin::static_files), not forbidden from being stored
        assert_eq!(
            get(&r, "/", None).await.headers()[header::CACHE_CONTROL],
            "no-cache"
        );
        let secure = get(&r, "/", Some("https")).await;
        assert!(
            secure.headers()[header::STRICT_TRANSPORT_SECURITY]
                .to_str()
                .unwrap()
                .starts_with("max-age=")
        );
    }
}
