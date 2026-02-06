"""
Configuration loader for the Eval Worker.

Mirrors the Go config pattern from 03-config.md, using Pydantic Settings
for environment variable parsing with SSM parameter resolution in non-local
environments.
"""

from __future__ import annotations

import os
from functools import lru_cache

import boto3
from pydantic import SecretStr
from pydantic_settings import BaseSettings


class Settings(BaseSettings):
    """Eval Worker configuration loaded from environment variables.

    In production (APP_ENV != 'local'), environment variables with an
    ``_SSM_PARAM`` suffix are resolved via AWS Systems Manager Parameter
    Store before constructing the settings object.
    """

    database_url: SecretStr
    forecast_bucket: str
    queue_url: str
    aws_region: str = "us-east-1"

    # Feature flags
    enable_threat_dedup: bool = True

    class Config:
        env_file = ".env"
        env_file_encoding = "utf-8"


def _resolve_ssm_params() -> None:
    """Scan environment variables for ``*_SSM_PARAM`` suffixes and replace
    them with the actual secret values fetched from AWS SSM Parameter Store.

    For example, if ``DATABASE_URL_SSM_PARAM=/watchpoint/prod/db-url`` is
    set, this function fetches that parameter and injects
    ``DATABASE_URL=<resolved_value>`` into the environment.
    """
    ssm_suffix = "_SSM_PARAM"
    params_to_resolve: dict[str, str] = {}

    for key, value in os.environ.items():
        if key.endswith(ssm_suffix):
            target_key = key[: -len(ssm_suffix)]
            params_to_resolve[target_key] = value

    if not params_to_resolve:
        return

    ssm = boto3.client("ssm", region_name=os.environ.get("AWS_REGION", "us-east-1"))

    # Batch fetch in groups of 10 (SSM API limit)
    param_names = list(params_to_resolve.values())
    for i in range(0, len(param_names), 10):
        batch = param_names[i : i + 10]
        response = ssm.get_parameters(Names=batch, WithDecryption=True)
        resolved = {p["Name"]: p["Value"] for p in response["Parameters"]}

        for target_key, param_name in params_to_resolve.items():
            if param_name in resolved:
                os.environ[target_key] = resolved[param_name]


@lru_cache(maxsize=1)
def load_settings() -> Settings:
    """Load and cache application settings.

    1. Check ``APP_ENV`` environment variable.
    2. If not ``local``, resolve SSM parameters into the environment.
    3. Construct and return the ``Settings`` object.
    """
    app_env = os.environ.get("APP_ENV", "local")
    if app_env != "local":
        _resolve_ssm_params()

    return Settings()  # type: ignore[call-arg]
