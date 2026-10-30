# The box scripts/windows-tests/run.sh runs Windows test binaries in:
# docker build -t agentry-winebox:local scripts/windows-tests.
# Wine maps Z: to / and follows links when it deletes, so a test that links
# to a system path must meet the container's filesystem, never the host's.
FROM registry.fedoraproject.org/fedora:46
RUN dnf install -y -q wine-core wine-common && dnf clean all
ENV WINEDEBUG=-all WINEDLLOVERRIDES="mscoree=;mshtml=" WINEPREFIX=/wine
RUN useradd -m tester && mkdir /wine && chown tester /wine
USER tester
RUN wineboot -i && wineserver -w
WORKDIR /work
