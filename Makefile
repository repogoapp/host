# The native transcript parser is opt-in: `make native` once, then any
# `go build`/`go run`/`go test` with `-tags native` links it. Without the tag
# the pure-Go parser is used and no Rust toolchain is needed.

NATIVE_LIB := internal/session/native/lib/librepogo_import.a
RUST_DIR   := internal/session/native/rust

.PHONY: native native-clean native-check native-check-local

native: $(NATIVE_LIB)

$(NATIVE_LIB): $(wildcard $(RUST_DIR)/src/*.rs) $(RUST_DIR)/Cargo.toml
	cd $(RUST_DIR) && cargo build --release --lib --no-default-features
	mkdir -p $(dir $@)
	cp $(RUST_DIR)/target/release/librepogo_import.a $@

# Proves the two parsers agree on every event of the provider fixtures; the
# stored content hashes depend on it.
native-check: native
	go test -tags native ./internal/agents -run "TestNativeFixtureParity|TestNativeUsageParity" -v

# The same comparison over this machine's own Claude and Codex history. Opt-in:
# it reads the developer's real sessions, so CI never runs it.
native-check-local: native
	go test -tags native ./internal/agents -run "TestNativeMatchesGoParser|TestNativeUsageParity" -native-local -v

native-clean:
	rm -f $(NATIVE_LIB)
