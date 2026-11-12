FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /app
COPY requirements.txt .
RUN pip install -r requirements.txt

COPY colorfix_solver ./colorfix_solver

RUN useradd --system --uid 10001 --no-create-home solver
USER 10001

# In a container the listener must be reachable from outside; COLORFIX_TOKEN is then mandatory
# (the process refuses to start without it). Prefer binding to an internal network only.
ENV HOST=0.0.0.0 PORT=18090
EXPOSE 18090
HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
  CMD python -c "import os,urllib.request;urllib.request.urlopen('http://127.0.0.1:%s/healthz'%os.environ.get('PORT','18090'),timeout=2)"

CMD ["python", "-m", "colorfix_solver"]
