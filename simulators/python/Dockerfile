FROM python:3.12-slim

WORKDIR /app
COPY pyproject.toml .
COPY simulator/ ./simulator/
RUN pip install --no-cache-dir .
RUN mkdir -p /app/results

CMD ["python", "-m", "simulator"]
