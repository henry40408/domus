//! Regenerates the README screenshots into `../screenshots/`.
//!
//!   cargo build && cd e2e && cargo run --bin screenshots
//!
//! It starts domus against a fake Hue Bridge, so no real house or private names
//! end up in the images. Font rendering and Chromium differ by machine: an
//! unrelated image changing in the diff means the browser changed, not the UI.

use std::path::{Path, PathBuf};
use std::time::Duration;

use anyhow::{Context, Result, bail};
use domus_e2e::fake_hue;
use domus_e2e::server::{Server, repo_root};
use serde_json::{Value, json};
use thirtyfour::prelude::*;

const USER: &str = "demo";
const PASSWORD: &str = "demo-password-123";
const GROUP: &str = "Garmin";

/// File stem, route, heading to wait for, viewport width and height.
const SHOTS: &[(&str, &str, &str, u32, u32)] = &[
    ("dashboard", "#dashboard", "Dashboard", 1280, 900),
    ("groups", "#groups", "Groups", 1280, 900),
    ("dashboard-mobile", "#dashboard", "Dashboard", 390, 844),
];

#[tokio::main]
async fn main() -> Result<()> {
    let out: PathBuf = repo_root().join("screenshots");
    std::fs::create_dir_all(&out)?;

    let (bridge, _bridge_task) = fake_hue::spawn().await?;
    let server = Server::start().await?;
    seed(&server.base_url, &server.setup_code, &bridge).await?;

    for theme in ["light", "dark"] {
        capture(&server.base_url, theme, &out).await?;
    }
    println!(
        "screenshots: wrote {} images to {}",
        SHOTS.len() * 2,
        out.display()
    );
    Ok(())
}

/// Sets up the admin, pairs the fake bridge and builds the demo group, all over the API.
async fn seed(base: &str, setup_code: &str, bridge: &str) -> Result<()> {
    let client = reqwest::Client::builder().cookie_store(true).build()?;
    let api = |path: &str| format!("{base}/api/domus{path}");
    client
        .post(api("/setup"))
        .json(&json!({"username": USER, "password": PASSWORD, "setup_code": setup_code}))
        .send()
        .await?
        .error_for_status()
        .context("creating the admin")?;
    client
        .post(api("/hue/pair"))
        .json(&json!({"ip": bridge}))
        .send()
        .await?
        .error_for_status()
        .context("pairing the fake bridge")?;

    let lights: Vec<Value> = client.get(api("/lights")).send().await?.json().await?;
    let members: Vec<String> = ["Ceiling", "Floor lamp", "Desk lamp", "Relax", "Nightlight"]
        .iter()
        .filter_map(|n| {
            lights
                .iter()
                .find(|e| e["name"].as_str().is_some_and(|name| name.ends_with(n)))
                .and_then(|e| e["entity_id"].as_str().map(str::to_owned))
        })
        .collect();
    if members.len() < 5 {
        bail!(
            "the fake bridge did not sync: only {} group members found",
            members.len()
        );
    }
    client
        .put(api(&format!("/groups/{GROUP}")))
        .json(&json!({"members": members, "expose_light": true}))
        .send()
        .await?
        .error_for_status()
        .context("creating the demo group")?;
    Ok(())
}

async fn capture(base: &str, theme: &str, out: &Path) -> Result<()> {
    let mut caps = DesiredCapabilities::chrome();
    caps.add_arg("--headless=new")?;
    caps.add_arg("--hide-scrollbars")?;
    caps.add_arg("--disable-dev-shm-usage")?;
    let driver = WebDriver::managed(caps).await.context(
        "could not start a browser session; a local Chrome or Chromium is required \
         (`brew install --cask ungoogled-chromium`)",
    )?;
    let result = shoot(&driver, base, theme, out).await;
    let quit = driver.quit().await;
    result?;
    Ok(quit?)
}

/// Waits for an element the page renders asynchronously.
async fn wait_for(driver: &WebDriver, by: By) -> Result<WebElement> {
    Ok(driver
        .query(by)
        .wait(Duration::from_secs(15), Duration::from_millis(100))
        .first()
        .await?)
}

async fn shoot(driver: &WebDriver, base: &str, theme: &str, out: &Path) -> Result<()> {
    driver
        .cdp()
        .send_raw(
            "Emulation.setEmulatedMedia",
            json!({"media": "screen", "features": [{"name": "prefers-color-scheme", "value": theme}]}),
        )
        .await?;

    // Log in through the real form once; the session cookie covers the rest.
    driver.goto(format!("{base}/")).await?;
    // The form renders after an async status fetch, so poll for it.
    wait_for(driver, By::Css("input[type=text]"))
        .await?
        .send_keys(USER)
        .await?;
    wait_for(driver, By::Css("input[type=password]"))
        .await?
        .send_keys(PASSWORD)
        .await?;
    wait_for(driver, By::Css("button.primary"))
        .await?
        .click()
        .await?;
    wait_for(driver, By::Css("nav"))
        .await
        .context("logging in")?;

    let suffix = if theme == "dark" { "-dark" } else { "" };
    for &(stem, route, title, width, height) in SHOTS {
        driver
            .cdp()
            .send_raw(
                "Emulation.setDeviceMetricsOverride",
                json!({"width": width, "height": height, "deviceScaleFactor": 1, "mobile": false}),
            )
            .await?;
        driver.goto(format!("{base}/{route}")).await?;
        // Going to the URL already shown reloads the page, so wait for this view's own heading.
        wait_for(
            driver,
            By::XPath(format!("//main/h2[normalize-space()='{title}']")),
        )
        .await?;
        // Let the page settle: live state and the watch preview paint after the shell.
        tokio::time::sleep(Duration::from_millis(800)).await;
        // The build's git version would change every image on every commit. Set through the
        // element's style, since the page's CSP refuses an injected <style>; done last
        // because a re-render replaces the element.
        driver
            .execute(
                "document.querySelectorAll('.version').forEach((e) => { e.style.display = 'none'; });",
                vec![],
            )
            .await?;
        let png = driver.screenshot_as_png().await?;
        std::fs::write(out.join(format!("{stem}{suffix}.png")), png)?;
    }
    Ok(())
}
