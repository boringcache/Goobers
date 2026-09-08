FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36

COPY source/ /opt/goobers-source/
COPY boringcache /usr/local/bin/boringcache
COPY pod-validation.test /opt/goobers-cache/pod-validation.test
COPY source/reference-workflows/boringcache/bin/ /opt/goobers-cache/
COPY source/reference-workflows/boringcache/validation/copilot-validation-fixture /opt/goobers-cache/copilot-validation-fixture
RUN mkdir -p /home/validation /workspace /cache /evidence \
    && chown -R 10001:10001 /home/validation /workspace /cache /evidence \
    && chmod 755 /usr/local/bin/boringcache /opt/goobers-cache/*

ENV HOME=/home/validation
ENV PATH=/opt/goobers-cache:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin
WORKDIR /workspace
USER 10001:10001
