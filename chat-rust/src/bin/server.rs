use std::sync::Arc;
use clap::Parser;
use anyhow::Result;

use chat::server::Server;

#[derive(Parser)]
#[command(name = "server", about = "RustChat TCP server")]
struct Args {
    /// TCP address to listen on
    #[arg(long, default_value = "0.0.0.0:8080")]
    addr: String,

    /// Directory for persistent storage
    #[arg(long, default_value = "./data")]
    data: String,

    /// Number of message-persistence worker tasks
    #[arg(long, default_value_t = 4)]
    workers: usize,
}

#[tokio::main]
async fn main() -> Result<()> {
    let args = Args::parse();

    let srv = Arc::new(Server::new(&args.data, args.workers)?);

    // Graceful shutdown on Ctrl-C
    tokio::spawn(async move {
        tokio::signal::ctrl_c().await.ok();
        eprintln!("[server] shutting down…");
        std::process::exit(0);
    });

    srv.listen_and_serve(&args.addr).await?;
    Ok(())
}
