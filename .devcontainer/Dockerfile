# Dev container for golang-classads.
#
# Modeled on ../golang-htcondor/Dockerfile (Rocky Linux 9 + Go + HTCondor),
# trimmed of the Node/frontend bits this repo does not need and extended with
# the HTCondor *Python* ClassAd bindings. Those bindings wrap the same C++
# ClassAd library that HTCondor ships, so a fuzzer can evaluate the same
# expression in the Go implementation and the reference C++/Python
# implementation and diff the results.
FROM rockylinux:9

# Set environment variables
ENV GO_VERSION=1.25.7 \
    GOPATH=/go \
    PATH=/usr/local/go/bin:/go/bin:$PATH \
    CONDOR_CONFIG=/etc/condor/condor_config

# Install basic development tools and dependencies
RUN dnf update -y && \
    dnf install -y \
    wget \
    git \
    gcc \
    gcc-c++ \
    make \
    tar \
    which \
    procps-ng \
    vim \
    sudo \
    jq \
    python3 \
    python3-pip \
    python3-devel \
    && dnf clean all

# Install Go
RUN ARCH=$(uname -m) && \
    if [ "$ARCH" = "aarch64" ]; then GOARCH="arm64"; else GOARCH="amd64"; fi && \
    wget -q https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz && \
    tar -C /usr/local -xzf go${GO_VERSION}.linux-${GOARCH}.tar.gz && \
    rm go${GO_VERSION}.linux-${GOARCH}.tar.gz

# Install golangci-lint (matches the version used in golang-htcondor CI)
RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b /usr/local/bin v2.6.1

# Install pre-commit (Python-based; repo ships a .pre-commit-config.yaml)
RUN pip3 install --no-cache-dir pre-commit

# Add HTCondor repository
RUN dnf install -y 'dnf-command(config-manager)' && \
    dnf config-manager --set-enabled crb && \
    dnf install -y epel-release && \
    cd /etc/yum.repos.d && \
    wget https://htcss-downloads.chtc.wisc.edu/repo/25.x/htcondor-release-current.el9.noarch.rpm && \
    dnf install -y htcondor-release-current.el9.noarch.rpm && \
    dnf clean all

# Install HTCondor and its Python bindings.
#
# python3-condor provides the `classad` / `classad2` Python modules, which are
# thin wrappers over HTCondor's reference C++ ClassAd implementation. This is
# the "ground truth" the Go library is fuzzed against.
#
# condor-devel provides the C++ ClassAd headers (/usr/include/classad/*.h) so
# the differential fuzzer can also link libclassad directly via cgo (see
# fuzz/oracle/cgo). The linker needs unversioned libclassad.so and
# libcondor_utils.so symlinks (the latter holds ClassAdReconfig, which registers
# HTCondor's extension functions). The versioned SONAME is recorded in the
# resulting binary, so these symlinks make the cgo link version-independent
# without changing what is loaded at runtime.
RUN dnf install -y \
    'condor >= 25.7.2' \
    python3-condor \
    condor-devel \
    && dnf clean all && \
    ln -sf "$(ldconfig -p | awk '/libclassad\.so/{print $NF; exit}')" /usr/lib64/libclassad.so && \
    ln -sf "$(ldconfig -p | awk '/libcondor_utils.*\.so/{print $NF; exit}')" /usr/lib64/libcondor_utils.so

# Sanity-check that the reference Python ClassAd bindings import cleanly.
RUN python3 -c "import classad2 as classad; ad = classad.parseAds('[ x = 1 + 2 ]'); print('classad2 bindings OK')" || \
    python3 -c "import classad; print('classad bindings OK')"

# Create workspace directory
WORKDIR /workspace

# Create a non-root user for development (useful for Codespaces)
RUN useradd -m -s /bin/bash -u 1000 vscode && \
    echo "vscode ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers.d/vscode && \
    mkdir -p /go && \
    chown -R vscode:vscode /go /workspace

# Switch to non-root user
USER vscode

# Pre-install common Go tooling (speeds up first build)
RUN go install golang.org/x/tools/gopls@latest && \
    go install github.com/go-delve/delve/cmd/dlv@latest && \
    go install honnef.co/go/tools/cmd/staticcheck@latest

WORKDIR /workspace

# Default command
CMD ["/bin/bash"]
