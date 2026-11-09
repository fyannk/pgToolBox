# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# The image the Playwright suite runs in, locally and in CI (scripts/e2e-docker.sh
# builds it): the screenshot baselines are rendered here, so their fonts do not
# depend on the machine. Playwright's own image, at the version of
# @playwright/test in package.json, by digest (Dependabot proposes the bumps; a
# bump re-renders the baselines: make ui-e2e-update). pnpm is the version
# mise.toml pins, from its release, checked by sha256 (hack/check-toolchain.sh).
FROM mcr.microsoft.com/playwright:v1.64.1-noble@sha256:2f05f8a734509a7b3f26a912639488ee08f58a7b292d14d62271bcff9038768f
ADD --checksum=sha256:565ff81174a14f6f7bd5d84c1b17e51031a80230d370a1fe24be5d16bd02a825 https://github.com/pnpm/pnpm/releases/download/v12.16.1/pnpm-linux-x64.tar.gz /tmp/pnpm.tar.gz
RUN mkdir /opt/pnpm && tar -xzf /tmp/pnpm.tar.gz -C /opt/pnpm && rm /tmp/pnpm.tar.gz \
    && ln -s /opt/pnpm/pnpm /usr/local/bin/pnpm && pnpm --version
ENV CI=1 npm_config_verify_deps_before_run=false pnpm_config_verify_deps_before_run=false
