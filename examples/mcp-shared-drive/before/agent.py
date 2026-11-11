"""The "before": a plain agent with no OAP between it and the MCP server.

It connects straight to the filesystem server and lets Claude call its tools
with the server's one credential. Nothing checks who is asking, or which file
a call touches.

    python agent.py "Draft the Q3 eng budget update using the template in /shared/eng."
"""

import asyncio
import json
import os
import sys
from pathlib import Path

import yaml
from anthropic import AsyncAnthropic
from anthropic.lib.tools.mcp import async_mcp_tool
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client

# The same six tools the OAP agent allowlists, so both halves offer Claude the
# same set.
ALLOWED = {"list_allowed_directories", "list_directory", "read_text_file",
           "get_file_info", "write_file", "create_directory"}

PROMPT_FILE = Path(__file__).resolve().parent.parent / "manifests" / "system-prompt.yaml"


def load_system_prompt() -> str:
    """The OAP agent's system prompt, read from its ConfigMap."""
    return yaml.safe_load(PROMPT_FILE.read_text())["data"]["system.md"]


async def load_tools(session: ClientSession) -> list:
    listed = await session.list_tools()
    return [async_mcp_tool(t, session) for t in listed.tools if t.name in ALLOWED]


async def run(prompt: str, url: str, model: str) -> None:
    async with streamable_http_client(url) as (read, write):
        async with ClientSession(read, write) as session:
            await session.initialize()
            runner = AsyncAnthropic().beta.messages.tool_runner(
                model=model,
                max_tokens=16000,
                system=load_system_prompt(),
                messages=[{"role": "user", "content": prompt}],
                tools=await load_tools(session),
            )
            async for message in runner:
                for block in message.content:
                    if block.type == "tool_use":
                        print(f"→ {block.name} {json.dumps(block.input)}")
                    elif block.type == "text":
                        print(block.text)


if __name__ == "__main__":
    asyncio.run(run(
        sys.argv[1],
        os.environ.get("MCP_URL", "http://localhost:8000/mcp"),
        os.environ.get("DEMO_MODEL", "claude-opus-4-8"),
    ))
