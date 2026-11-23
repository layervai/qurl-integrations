from fastapi import FastAPI, Request

app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)


@app.middleware("http")
async def count_app_requests(request: Request, call_next):
    print("APP_REQUEST", flush=True)
    return await call_next(request)


@app.get("/")
def read_private_app():
    return {"message": "This request reached the private FastAPI app."}
