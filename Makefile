# Makefile for mod_socket_handoff
#
# Build: make
# Install: sudo make install
# Clean: make clean

APXS ?= apxs
MODULE = mod_socket_handoff

.PHONY: all install clean test

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
	@echo "After installation:"
	@echo "  1. Add to Apache config: LoadModule socket_handoff_module modules/mod_socket_handoff.so"
	@echo "  2. Configure: SocketHandoffEnabled On"
	@echo "  3. Restart Apache"
