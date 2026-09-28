ARG NODE_IMAGE=node:24.13.0-bookworm
ARG GRAFANA_BASE_IMAGE=grafana/grafana:12.2.9
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS metrics-plugin
WORKDIR /build
COPY .github/scripts/grafana-plugin-build.mjs /build/grafana-plugin-build.mjs
COPY deploy/grafana/metrics-drilldown.patch /build/metrics-drilldown.patch
RUN node /build/grafana-plugin-build.mjs

FROM ${GRAFANA_BASE_IMAGE}
USER root
ENV GF_PATHS_PLUGINS=/opt/cerberus-plugins
ENV GF_PLUGINS_PREINSTALL_DISABLED=true
ENV GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS=grafana-metricsdrilldown-app
RUN mkdir -p /opt/cerberus-plugins
RUN grafana cli --pluginsDir /opt/cerberus-plugins plugins install grafana-exploretraces-app 2.1.0
RUN grafana cli --pluginsDir /opt/cerberus-plugins plugins install grafana-lokiexplore-app 2.5.0
RUN grafana cli --pluginsDir /opt/cerberus-plugins plugins install grafana-pyroscope-app 2.2.0
COPY --from=metrics-plugin /build/source/dist /opt/cerberus-plugins/grafana-metricsdrilldown-app
COPY --from=metrics-plugin /build/metrics-drilldown-source.tar.gz /usr/share/grafana/plugin-source/metrics-drilldown-source.tar.gz
COPY deploy/grafana/metrics-drilldown.patch /usr/share/grafana/plugin-source/metrics-drilldown.patch
COPY deploy/grafana/README.md /usr/share/grafana/plugin-source/README.md
LABEL org.opencontainers.image.source="https://github.com/tsouza/cerberus"
USER 472
