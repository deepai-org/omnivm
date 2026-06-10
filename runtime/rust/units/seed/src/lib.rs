//! Seed unit — validates the export/ABI pipeline at image build time.

fn seed_ping() -> &'static str {
    "seed"
}

omnivm::export_fn!(OmniVMCall_seed_ping, seed_ping, 0);
omnivm::unit_abi_marker!();
