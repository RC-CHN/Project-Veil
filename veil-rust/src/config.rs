use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use rand::Rng;
use serde::Deserialize;
use std::{io, time::Duration};

#[derive(Clone, Copy, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Range {
    pub min: usize,
    pub max: usize,
}
impl Range {
    pub fn sample(self) -> usize {
        rand::rng().random_range(self.min..=self.max)
    }
    pub fn narrowed(self) -> Self {
        let c = self.sample();
        Self {
            min: self.min.max(c - c / 4),
            max: self.max.min(c + c / 4),
        }
    }
}

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Traffic {
    pub version: u8,
    pub quantum_bytes: Range,
    pub startup_bytes: Range,
    pub startup_writes: Range,
    pub padding_limit: Range,
    pub padding_budget: Range,
    pub credit_blocks: Range,
    pub control_padding_limit: Range,
}
impl Default for Traffic {
    fn default() -> Self {
        Self {
            version: 1,
            quantum_bytes: Range {
                min: 8192,
                max: 32768,
            },
            startup_bytes: Range {
                min: 512,
                max: 2048,
            },
            startup_writes: Range { min: 2, max: 6 },
            padding_limit: Range { min: 0, max: 0 },
            padding_budget: Range { min: 0, max: 0 },
            credit_blocks: Range { min: 16, max: 64 },
            control_padding_limit: Range { min: 0, max: 0 },
        }
    }
}
impl Traffic {
    pub fn validate(&self) -> io::Result<()> {
        if self.version != 1 {
            return Err(io::Error::other("unsupported traffic version"));
        }
        for (r, lo, hi) in [
            (self.quantum_bytes, 4096, 32768),
            (self.startup_bytes, 128, 8192),
            (self.startup_writes, 0, 12),
            (self.credit_blocks, 16, 64),
        ] {
            if r.min < lo || r.max > hi || r.min > r.max {
                return Err(io::Error::other("traffic range outside bounds"));
            }
        }
        // rustls exposes no equivalent per-record budget API. Fail explicitly
        // instead of silently dropping a deployment's requested camouflage.
        for r in [
            self.padding_limit,
            self.padding_budget,
            self.control_padding_limit,
        ] {
            if r.min != 0 || r.max != 0 {
                return Err(io::Error::other(
                    "TLS padding budgets are not supported by this experimental transport",
                ));
            }
        }
        Ok(())
    }
}

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Tls {
    pub mode: String,
    pub server_name: String,
    #[serde(default)]
    pub certificate: String,
    #[serde(default)]
    pub private_key_file: String,
    #[serde(default)]
    pub ca_file: String,
    #[serde(default)]
    pub ca_pem: String,
    #[serde(default)]
    pub record_padding: bool,
}

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Config {
    pub role: String,
    pub listen: String,
    #[serde(default)]
    pub server: String,
    pub secret: String,
    pub tls: Tls,
    #[serde(default)]
    pub traffic: Traffic,
    #[serde(default = "connections")]
    pub max_connections: usize,
    #[serde(default = "max_idle")]
    pub max_idle: usize,
    #[serde(default = "idle")]
    pub idle_seconds: u64,
    #[serde(default = "pool_idle")]
    pub pool_seconds: u64,
}
fn connections() -> usize {
    64
}
fn max_idle() -> usize {
    8
}
fn idle() -> u64 {
    120
}
fn pool_idle() -> u64 {
    20
}

impl Config {
    pub fn key(&self) -> io::Result<[u8; 32]> {
        URL_SAFE_NO_PAD
            .decode(&self.secret)
            .map_err(io::Error::other)?
            .try_into()
            .map_err(|_| io::Error::other("secret must be 32 bytes"))
    }
    pub fn validate(&self) -> io::Result<()> {
        self.key()?;
        self.traffic.validate()?;
        if !matches!(self.role.as_str(), "client" | "server")
            || self.tls.mode != "tls"
            || self.tls.server_name.is_empty()
            || self.tls.record_padding
            || self.max_connections == 0
            || self.max_connections > 4096
            || self.max_idle > self.max_connections
            || !(1..=86400).contains(&self.idle_seconds)
            || !(1..=86400).contains(&self.pool_seconds)
        {
            return Err(io::Error::other(
                "invalid configuration; Rust currently supports Veil v0.3/T without TLS padding",
            ));
        }
        Ok(())
    }
    pub fn idle(&self) -> Duration {
        Duration::from_secs(self.idle_seconds)
    }
}
