//! Independent experimental Veil v0.3/T implementation.
//! The mux accepts any authenticated asynchronous byte transport; TLS and proxy
//! listeners are separate from stream state and flow control.
pub mod config;
pub mod mux;
pub mod proxy;
pub mod transport;
pub mod wire;
