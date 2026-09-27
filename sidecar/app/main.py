import io
import os
from pathlib import Path

from dotenv import load_dotenv
from fastapi import FastAPI, File, HTTPException, UploadFile

load_dotenv(Path(__file__).resolve().parents[2] / ".env")

app = FastAPI(title="Parser Sidecar", version="0.1.0")

TEXT_EXTENSIONS = {".md", ".txt", ".csv"}
RICH_EXTENSIONS = {".pdf", ".docx", ".doc", ".pptx", ".xlsx", ".html", ".htm"}


@app.get("/healthz")
def healthz() -> dict:
    return {"status": "ok"}


@app.post("/parse")
async def parse(file: UploadFile = File(...)) -> dict:
    filename = file.filename or ""
    extension = os.path.splitext(filename)[1].lower()
    data = await file.read()
    if not data:
        raise HTTPException(status_code=400, detail="empty file")

    if extension in TEXT_EXTENSIONS:
        return {"filename": filename, "markdown": data.decode("utf-8", errors="replace")}

    if extension not in RICH_EXTENSIONS:
        raise HTTPException(status_code=415, detail=f"unsupported extension: {extension}")

    try:
        from markitdown import MarkItDown
    except ImportError as exc:
        raise HTTPException(status_code=503, detail="markitdown is not installed") from exc

    try:
        result = MarkItDown().convert_stream(io.BytesIO(data), file_extension=extension)
    except Exception as exc:
        raise HTTPException(status_code=422, detail=f"parse failed: {exc}") from exc

    text = getattr(result, "markdown", None) or getattr(result, "text_content", "")
    if not text:
        raise HTTPException(status_code=422, detail="no text extracted")
    return {"filename": filename, "markdown": text}
