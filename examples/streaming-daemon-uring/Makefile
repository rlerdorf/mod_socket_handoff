# Makefile for streaming-daemon-uring
#
# Build a high-performance C daemon using io_uring for async I/O.
#
# Requirements:
#   - liburing (liburing-dev on Debian/Ubuntu)
#   - Linux 5.1+ (5.19+ recommended for multishot accept)
#   - libcurl (libcurl4-openssl-dev) for OpenAI backend
#
# Usage:
#   make                  - Build with mock backend (default)
#   make BACKEND=openai   - Build with OpenAI backend (requires libcurl)
#   make clean            - Remove build artifacts
#   make install          - Install to /usr/local/bin (requires sudo)

CC ?= gcc
CFLAGS = -Wall -Wextra -Werror -O2 -g
LDFLAGS = -luring -lpthread

# Backend selection: mock (default) or openai
BACKEND ?= mock

# Core source files (excluding backend)
CORE_SRCS = daemon.c ring.c server.c handoff.c connection.c streaming.c json.c pool.c metrics.c config.c write_buffer.c

# Backend-specific configuration
ifeq ($(BACKEND),openai)
    BACKEND_SRC = backend_openai.c curl_manager.c
    # Use the newer libcurl.so.4 (OpenSSL/8.14.1) which has working HTTP/2 multiplexing
    # The older libcurl-nss (7.88.1) has a bug with h2c multiplexing
    CURL_LIB ?= $(shell test -f /lib/x86_64-linux-gnu/libcurl.so.4 && echo "/lib/x86_64-linux-gnu/libcurl.so.4" || echo "-lcurl")
    ifeq ($(CURL_LIB),-lcurl)
        LDFLAGS += -lcurl
    else
        LDFLAGS += $(CURL_LIB)
    endif
    CFLAGS += -DBACKEND_OPENAI
else
    BACKEND_SRC = backend_mock.c
endif

SRCS = $(CORE_SRCS) $(BACKEND_SRC)
OBJS = $(SRCS:.c=.o)
DEPS = $(wildcard *.h)

# Target binary
TARGET = streaming-daemon-uring

# Installation prefix
PREFIX ?= /usr/local

.PHONY: all clean install uninstall

all: $(TARGET)

$(TARGET): $(OBJS)
	$(CC) $(CFLAGS) -o $@ $(OBJS) $(LDFLAGS)

%.o: %.c $(DEPS)
	$(CC) $(CFLAGS) -c -o $@ $<

clean:
	rm -f *.o $(TARGET)

install: $(TARGET)
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 755 $(TARGET) $(DESTDIR)$(PREFIX)/bin/

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(TARGET)

# Development targets
.PHONY: debug sanitize check

# Debug build with no optimization
debug: CFLAGS = -Wall -Wextra -O0 -g3 -DDEBUG
debug: clean $(TARGET)

# Build with AddressSanitizer
sanitize: CFLAGS = -Wall -Wextra -O1 -g -fsanitize=address -fno-omit-frame-pointer
sanitize: LDFLAGS += -fsanitize=address
sanitize: clean $(TARGET)

# Static analysis
check:
	cppcheck --enable=all --suppress=missingIncludeSystem $(SRCS)

# Show kernel version and io_uring support
.PHONY: check-kernel
check-kernel:
	@echo "Kernel version: $$(uname -r)"
	@echo "io_uring support:"
	@grep -q io_uring /proc/filesystems 2>/dev/null && echo "  io_uring filesystem: yes" || echo "  io_uring filesystem: no"
	@test -e /proc/sys/kernel/io_uring_disabled && echo "  io_uring disabled: $$(cat /proc/sys/kernel/io_uring_disabled)" || echo "  io_uring disabled: (no control file)"
