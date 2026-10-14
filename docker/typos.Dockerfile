FROM python:3.14-slim-bookworm@sha256:48b13b003dda20b16f9442b8475aa05fe21bf6579a8c881db92ffb4d8fd20f83
COPY typos-requirements.txt /tmp/typos-requirements.txt
RUN pip install --no-cache-dir --disable-pip-version-check --require-hashes -r /tmp/typos-requirements.txt \
  && rm /tmp/typos-requirements.txt \
  && groupadd --gid 10001 dev && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin dev
USER 10001:10001
WORKDIR /work
ENTRYPOINT ["typos"]
