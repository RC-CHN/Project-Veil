use std::{fs, io};
use veil_rust::{config::Config, proxy};
#[tokio::main(flavor = "current_thread")]
async fn main() -> io::Result<()> {
    let args: Vec<_> = std::env::args().collect();
    if args.len() != 3 || args[1] != "-config" {
        return Err(io::Error::other("usage: veil-rust -config config.json"));
    }
    let config: Config = serde_json::from_slice(&fs::read(&args[2])?).map_err(io::Error::other)?;
    proxy::serve(config).await
}
