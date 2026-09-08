# Build context: this directory plus verified Linux binaries named
# boringcache-v1.20.5 and goobers. GOOBERS_IMAGE is the operator's existing
# runner image, pinned by digest and containing its normal Go and Copilot tools.
ARG GOOBERS_IMAGE
FROM ${GOOBERS_IMAGE}

COPY --chmod=0755 boringcache-v1.20.5 /usr/local/bin/boringcache
COPY --chmod=0755 goobers /opt/goobers/bin/goobers-real
COPY --chmod=0755 bin/ /opt/goobers-cache/
ENV PATH="/opt/goobers-cache:${PATH}"

# Preserve the base image's user and entrypoint. The dispatcher invokes the
# goobers executable on PATH, which supervises only __dispatch-exec.
