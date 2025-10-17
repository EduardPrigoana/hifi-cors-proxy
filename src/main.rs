use axum::{
    body::Body,
    extract::{Request, State},
    http::{HeaderMap, HeaderValue, Method, StatusCode, Uri},
    response::{IntoResponse, Response},
    routing::any,
    Router,
};
use moka::future::Cache;
use parking_lot::RwLock;
use serde::Deserialize;
use std::{sync::Arc, time::Duration};
use tower_http::cors::{Any, CorsLayer};
use tracing::{error, info, warn};

const INSTANCES_URL: &str = "https://raw.githubusercontent.com/EduardPrigoana/monochrome/refs/heads/main/instances.json";
const SPEED_TEST_PATH: &str = "/search/?s=kanye";
const CACHE_TTL_HOURS: u64 = 2;
const RETEST_INTERVAL_HOURS: u64 = 1;
const REQUEST_TIMEOUT_SECS: u64 = 10;
const SPEED_TEST_TIMEOUT_SECS: u64 = 5;

#[derive(Clone)]
struct AppState {
    instances: Arc<RwLock<Vec<String>>>,
    cache: Cache<String, CachedResponse>,
    client: reqwest::Client,
}

#[derive(Clone)]
struct CachedResponse {
    status: StatusCode,
    headers: HeaderMap,
    body: bytes::Bytes,
}

#[derive(Deserialize)]
struct Instances(Vec<String>);

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .with_max_level(tracing::Level::INFO)
        .init();

    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(REQUEST_TIMEOUT_SECS))
        .pool_max_idle_per_host(10)
        .pool_idle_timeout(Duration::from_secs(90))
        .http2_adaptive_window(true)
        .http2_keep_alive_interval(Duration::from_secs(30))
        .build()
        .expect("Failed to create HTTP client");

    let cache = Cache::builder()
        .time_to_live(Duration::from_secs(CACHE_TTL_HOURS * 3600))
        .max_capacity(10_000)
        .build();

    let state = AppState {
        instances: Arc::new(RwLock::new(Vec::new())),
        cache,
        client,
    };

    info!("Running initial speed test...");
    run_speed_test(state.clone()).await;

    let state_clone = state.clone();
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(Duration::from_secs(RETEST_INTERVAL_HOURS * 3600));
        interval.tick().await;
        
        loop {
            interval.tick().await;
            info!("Running periodic speed test...");
            run_speed_test(state_clone.clone()).await;
        }
    });

    let cors = CorsLayer::new()
        .allow_origin(Any)
        .allow_methods(Any)
        .allow_headers(Any)
        .expose_headers(Any);

    let app = Router::new()
        .fallback(any(proxy_handler))
        .layer(cors)
        .with_state(state);

    let listener = tokio::net::TcpListener::bind("0.0.0.0:3000")
        .await
        .expect("Failed to bind to port 3000");
    
    info!("Server running on http://0.0.0.0:3000");
    
    axum::serve(listener, app)
        .await
        .expect("Server failed");
}

async fn run_speed_test(state: AppState) {
    let instances = match fetch_instances(&state.client).await {
        Ok(inst) => inst,
        Err(e) => {
            error!("Failed to fetch instances: {}", e);
            return;
        }
    };

    if instances.is_empty() {
        error!("No instances found");
        return;
    }

    info!("Testing {} instances...", instances.len());

    let mut handles = Vec::new();
    for instance in instances.iter() {
        let client = state.client.clone();
        let instance = instance.clone();
        
        handles.push(tokio::spawn(async move {
            test_instance_speed(&client, &instance).await
        }));
    }

    let mut results: Vec<(String, Duration)> = Vec::new();
    for (i, handle) in handles.into_iter().enumerate() {
        if let Ok(Ok(Some(duration))) = handle.await {
            results.push((instances[i].clone(), duration));
        }
    }

    results.sort_by_key(|(_, duration)| *duration);

    let ranked: Vec<String> = results.iter().map(|(url, _)| url.clone()).collect();
    
    if !ranked.is_empty() {
        info!("Speed test complete. Fastest: {} ({:?})", ranked[0], results[0].1);
        *state.instances.write() = ranked;
    } else {
        error!("All instances failed speed test");
    }
}

async fn fetch_instances(client: &reqwest::Client) -> Result<Vec<String>, Box<dyn std::error::Error>> {
    let response = client.get(INSTANCES_URL).send().await?;
    let instances: Instances = response.json().await?;
    Ok(instances.0)
}

async fn test_instance_speed(client: &reqwest::Client, instance: &str) -> Result<Option<Duration>, Box<dyn std::error::Error>> {
    let url = format!("{}{}", instance, SPEED_TEST_PATH);
    
    let start = std::time::Instant::now();
    
    let result = tokio::time::timeout(
        Duration::from_secs(SPEED_TEST_TIMEOUT_SECS),
        client.get(&url).send()
    ).await;

    match result {
        Ok(Ok(response)) if response.status().is_success() => {
            let duration = start.elapsed();
            Ok(Some(duration))
        }
        _ => Ok(None),
    }
}

async fn proxy_handler(
    State(state): State<AppState>,
    method: Method,
    uri: Uri,
    headers: HeaderMap,
    body: Body,
) -> Result<Response, StatusCode> {
    let path = uri.path();
    let query = uri.query().unwrap_or("");
    let full_path = if query.is_empty() {
        path.to_string()
    } else {
        format!("{}?{}", path, query)
    };

    let cache_key = format!("{}:{}", method.as_str(), full_path);

    if method == Method::GET {
        if let Some(cached) = state.cache.get(&cache_key).await {
            info!("Cache hit: {}", full_path);
            return Ok(build_response(cached));
        }
    }

    let instances = state.instances.read().clone();
    
    if instances.is_empty() {
        error!("No instances available");
        return Err(StatusCode::SERVICE_UNAVAILABLE);
    }

    let body_bytes = match axum::body::to_bytes(body, usize::MAX).await {
        Ok(bytes) => bytes,
        Err(_) => return Err(StatusCode::BAD_REQUEST),
    };

    for instance in instances.iter() {
        let url = format!("{}{}", instance, full_path);
        
        let mut request = state.client.request(method.clone(), &url);
        
        for (key, value) in headers.iter() {
            if key != "host" {
                request = request.header(key, value);
            }
        }

        if !body_bytes.is_empty() {
            request = request.body(body_bytes.clone());
        }

        match request.send().await {
            Ok(response) => {
                let status = response.status();
                
                if status.is_success() || status.is_redirection() {
                    let mut response_headers = HeaderMap::new();
                    for (key, value) in response.headers().iter() {
                        response_headers.insert(key.clone(), value.clone());
                    }
                    
                    match response.bytes().await {
                        Ok(response_body) => {
                            let cached = CachedResponse {
                                status,
                                headers: response_headers.clone(),
                                body: response_body.clone(),
                            };

                            if method == Method::GET && status.is_success() {
                                state.cache.insert(cache_key.clone(), cached.clone()).await;
                            }

                            info!("Success from {}: {} {}", instance, method, full_path);
                            return Ok(build_response(cached));
                        }
                        Err(e) => {
                            warn!("Failed to read body from {}: {}", instance, e);
                            continue;
                        }
                    }
                } else {
                    warn!("Non-success status {} from {}", status, instance);
                    continue;
                }
            }
            Err(e) => {
                warn!("Request failed to {}: {}", instance, e);
                continue;
            }
        }
    }

    error!("All instances failed for {} {}", method, full_path);
    Err(StatusCode::NOT_FOUND)
}

fn build_response(cached: CachedResponse) -> Response {
    let mut response = Response::builder()
        .status(cached.status);

    let headers = response.headers_mut().unwrap();
    for (key, value) in cached.headers.iter() {
        headers.insert(key.clone(), value.clone());
    }

    response.body(Body::from(cached.body)).unwrap()
}
