FROM python:3.15-slim-bookworm@sha256:122756f3fe72c4f5d3d71897888204ee85774bc79de952555b8a474e1d0a5d53
COPY typos-requirements.txt /tmp/typos-requirements.txt
RUN pip install --no-cache-dir --disable-pip-version-check --require-hashes -r /tmp/typos-requirements.txt \
  && rm /tmp/typos-requirements.txt \
  && groupadd --gid 10001 dev && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin dev
USER 10001:10001
WORKDIR /work
ENTRYPOINT ["typos"]
