FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    REVERSE_BIND=0.0.0.0:8090

RUN useradd --system --no-create-home --shell /usr/sbin/nologin reverse
WORKDIR /app
# 只拷服务本体与网关客户端；不拷 .env 与 tests
# 运行时注入 AIG_BASE_URL / AIG_SECRET（必填）、AIG_KEY_ID / AIG_CALLER / REVERSE_MODEL_GROUP / REVERSE_ALLOWED_DEPLOYMENTS / AIG_WAIT_S（可选）
COPY serve_reverse.py aig_client.py /app/

USER reverse
EXPOSE 8090
# 容器内监听 0.0.0.0，仅限内部网络/compose network 暴露，不要映射到公网
CMD ["python3", "-I", "serve_reverse.py"]
