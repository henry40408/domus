//! `GET /health` for load balancers and uptime monitors, and `domus healthcheck` for the
//! container's own `HEALTHCHECK` (the distroless image has no shell or curl).

use axum::Json;
use axum::extract::State;
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use serde_json::json;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::time::Duration;

use crate::GIT_VERSION;
use crate::api::AppState;

/// Public and unauthenticated, so it says as little as possible: whether the database answers
/// and which build this is. The Hue Bridge is deliberately not checked; it being offline is
/// normal and must not get the container restarted.
pub async fn health(State(app): State<AppState>) -> Response {
    if app.core.store().ping().await {
        Json(json!({"status": "ok", "version": GIT_VERSION})).into_response()
    } else {
        (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(json!({"status": "error", "version": GIT_VERSION})),
        )
            .into_response()
    }
}

/// The address a local process reaches the server on: a wildcard bind is not connectable.
pub fn health_url(bind: SocketAddr) -> String {
    let ip = match bind.ip() {
        IpAddr::V4(ip) if ip.is_unspecified() => IpAddr::V4(Ipv4Addr::LOCALHOST),
        IpAddr::V6(ip) if ip.is_unspecified() => IpAddr::V6(Ipv6Addr::LOCALHOST),
        ip => ip,
    };
    format!("http://{}/health", SocketAddr::new(ip, bind.port()))
}

/// Asks the running server whether it is healthy; `Err` carries the reason.
pub async fn check(url: &str) -> Result<(), String> {
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(4))
        .build()
        .map_err(|e| e.to_string())?;
    let resp = client.get(url).send().await.map_err(|e| e.to_string())?;
    if resp.status() == reqwest::StatusCode::OK {
        Ok(())
    } else {
        Err(format!("{url} answered {}", resp.status()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wildcard_binds_are_reached_through_loopback() {
        let url = |s: &str| health_url(s.parse().unwrap());
        assert_eq!(url("0.0.0.0:8123"), "http://127.0.0.1:8123/health");
        assert_eq!(url("[::]:9000"), "http://[::1]:9000/health");
        assert_eq!(url("192.168.1.5:80"), "http://192.168.1.5:80/health");
    }

    #[tokio::test]
    async fn check_follows_the_server_status() {
        // nothing listens here: connection refused
        let closed = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = health_url(closed.local_addr().unwrap());
        drop(closed);
        assert!(check(&url).await.is_err());

        // a live server answering 200 is healthy, 503 is not
        for (status, ok) in [
            (StatusCode::OK, true),
            (StatusCode::SERVICE_UNAVAILABLE, false),
        ] {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let url = health_url(listener.local_addr().unwrap());
            let app = axum::Router::new()
                .route("/health", axum::routing::get(move || async move { status }));
            tokio::spawn(async move { axum::serve(listener, app).await });
            assert_eq!(check(&url).await.is_ok(), ok);
        }
    }
}
