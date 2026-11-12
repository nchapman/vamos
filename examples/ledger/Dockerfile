FROM python:3.13-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY ledger.py migrate.py ./
COPY bin ./bin
USER 10001:10001
EXPOSE 8080
CMD ["python", "ledger.py"]
