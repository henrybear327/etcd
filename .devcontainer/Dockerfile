FROM mcr.microsoft.com/devcontainers/go:1.25-bookworm

# Remove the expired Yarn repository to fix apt-get update issues
# The Yarn GPG key (23E7166788B63E1E) has expired, causing build failures
# Since this is a Go project, Yarn is not needed
RUN rm -f /etc/apt/sources.list.d/yarn.list
