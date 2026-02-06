# Makefile for mod_socket_handoff
#
# Build: make
# Install: sudo make install
# Clean: make clean

APXS ?= apxs
MODULE = mod_socket_handoff

.PHONY: all install clean test-go build-go test-rust test-daemons

all: $(MODULE).la

$(MODULE).la: $(MODULE).c
	$(APXS) -c $(MODULE).c

install: $(MODULE).la
	$(APXS) -i -a $(MODULE).la

clean:
	rm -rf .libs *.la *.lo *.slo *.o

# Enable the module (Debian/Ubuntu)
enable:
	@if [ -d /etc/apache2/mods-available ]; then \
		sudo cp apache/socket_handoff.load /etc/apache2/mods-available/; \
		sudo cp apache/socket_handoff.conf /etc/apache2/mods-available/; \
		sudo a2enmod socket_handoff; \
		echo "Module enabled. Run: sudo systemctl reload apache2"; \
	else \
		echo "Not a Debian-style Apache install. Manually add LoadModule directive."; \
	fi

# Disable the module (Debian/Ubuntu)
disable:
	@if [ -d /etc/apache2/mods-enabled ]; then \
		sudo a2dismod socket_handoff; \
		echo "Module disabled. Run: sudo systemctl reload apache2"; \
	fi

# Test compile only
test-compile: $(MODULE).la
	@echo "Compilation successful!"

# Build Go daemon
build-go:
	$(MAKE) -C examples/streaming-daemon-go build

# Test Go daemon
test-go:
	$(MAKE) -C examples/streaming-daemon-go test

# Test Rust daemon
test-rust:
	@echo "Running Rust daemon tests..."
	cd examples/streaming-daemon-rs && cargo test --lib

# Run all daemon tests
test-daemons: test-go test-rust
	@echo "All daemon tests passed!"

# Show help
help:
	@echo "mod_socket_handoff - Apache socket handoff output filter"
	@echo ""
	@echo "Usage:"
	@echo "  make            - Build the module"
	@echo "  make install    - Install the module"
	@echo "  make enable     - Enable module (Debian/Ubuntu)"
	@echo "  make disable    - Disable module (Debian/Ubuntu)"
	@echo "  make clean      - Remove build artifacts"
	@echo ""
	@echo "Testing:"
	@echo "  make build-go     - Build Go daemon"
	@echo "  make test-go      - Run Go daemon tests"
	@echo "  make test-rust    - Run Rust daemon tests"
	@echo "  make test-daemons - Run all daemon tests"
	@echo ""
	@echo "After installation:"
	@echo "  1. Add to Apache config: LoadModule socket_handoff_module modules/mod_socket_handoff.so"
	@echo "  2. Configure: SocketHandoffEnabled On"
	@echo "  3. Restart Apache"
