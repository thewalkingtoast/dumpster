#!/usr/bin/env python3
"""Screenshot the UI in both themes, at desktop and phone widths.

Needs a running instance and Playwright:

    python3 -m pip install playwright
    python3 -m playwright install chromium

If that download fails — a common one is

    EACCES: permission denied, mkdir '/opt/hermes/tools/__dirlock'

because PLAYWRIGHT_BROWSERS_PATH points into a directory owned by another user —
borrow any Chrome/Chromium that is already on the machine:

    CHROMIUM=/path/to/chrome python3 scripts/shots.py

Usage:

    APP_URL=http://localhost:8080 python3 scripts/shots.py
    APP_URL=https://dumpster.toastsnacks.dev python3 scripts/shots.py

Writes shots/{dark,light}-{desktop,mobile}.png. It rents a throwaway dumpster,
sends a few requests through several protocols so the page has rows to show, and
leaves it to expire.
"""

from __future__ import annotations

import json
import os
import pathlib
import re
import socket
import urllib.parse
import urllib.request

from playwright.sync_api import sync_playwright

APP_URL = os.environ.get("APP_URL", "http://localhost:8080").rstrip("/")
OUT = pathlib.Path(os.environ.get("OUT", "shots"))
CHROMIUM = os.environ.get("CHROMIUM")

VIEWPORTS = {"desktop": (1440, 1000), "mobile": (393, 852)}  # mobile = iPhone 16 Pro Max CSS width


def post(path: str, form: dict[str, str], origin: str | None = None) -> str:
    data = urllib.parse.urlencode(form).encode()
    req = urllib.request.Request(APP_URL + path, data=data, method="POST")
    req.add_header("Content-Type", "application/x-www-form-urlencoded")
    if origin:
        req.add_header("Origin", origin)
    with urllib.request.urlopen(req) as res:  # 303 is followed automatically
        return res.geturl()


def send_something(dumpster_url: str) -> None:
    """Give the page some rows: http, tcp, udp."""
    with urllib.request.urlopen(
        urllib.request.Request(
            dumpster_url + "/api/orders",
            data=json.dumps({"hello": "dumpster", "n": 1}).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
    ):
        pass

    host = urllib.parse.urlparse(APP_URL).hostname or "localhost"
    uid = dumpster_url.rsplit("/", 1)[-1]
    for port, payload, kind in ((8082, f"{uid}\nGET /temp HTTP/1.1\n", "tcp"),):
        try:
            with socket.create_connection((host, port), timeout=3) as s:
                s.sendall(payload.encode())
        except OSError as e:
            print(f"  ({kind} skipped: {e})")
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as u:
            u.sendto(f"{uid}|datagram payload".encode(), (host, 8083))
    except OSError as e:
        print(f"  (udp skipped: {e})")


def main() -> None:
    with urllib.request.urlopen(APP_URL + "/") as res:
        page = res.read().decode()
    token = re.search(r'name="csrf" value="([^"]+)"', page)
    if not token:
        raise SystemExit("no csrf field on the landing page; is APP_URL right?")

    # Origin: null is what Firefox sends for a same-origin form post, and the
    # server accepts it — using it here keeps the script browser-honest.
    dumpster = post("/rent", {"csrf": token.group(1)}, origin="null")
    print(f"rented {dumpster}")
    send_something(dumpster)

    OUT.mkdir(parents=True, exist_ok=True)
    with sync_playwright() as p:
        # Spelled out rather than a kwargs dict: the types stay obvious to both
        # a reader and a type checker.
        if CHROMIUM:
            browser = p.chromium.launch(executable_path=CHROMIUM, args=["--no-sandbox"])
        else:
            browser = p.chromium.launch(args=["--no-sandbox"])
        # The landing page carries the switch too, so shoot it as well.
        for theme in ("dark", "light"):
            for name, (w, h) in VIEWPORTS.items():
                ctx = browser.new_context(
                    viewport={"width": w, "height": h},
                    device_scale_factor=2 if w < 600 else 1,
                )
                ctx.add_init_script(f"localStorage.setItem('dumpster-theme', '{theme}')")
                page = ctx.new_page()
                page.goto(APP_URL + "/", wait_until="networkidle")
                path = OUT / f"{theme}-landing-{name}.png"
                page.screenshot(path=str(path))
                print(f"  {path}")
                ctx.close()

        for theme in ("dark", "light"):
            for name, (w, h) in VIEWPORTS.items():
                ctx = browser.new_context(
                    viewport={"width": w, "height": h},
                    device_scale_factor=2 if w < 600 else 1,
                )
                # Seed the stored preference so the shot does not depend on the
                # order the pages were visited in.
                ctx.add_init_script(
                    f"localStorage.setItem('dumpster-theme', '{theme}')"
                )
                page = ctx.new_page()
                page.goto(dumpster, wait_until="networkidle")
                row = page.query_selector(".row")
                if row:
                    row.click()  # open one record so the detail panes are visible
                    page.wait_for_timeout(250)
                    page.mouse.move(0, 0)  # drop the hover wash the click leaves
                path = OUT / f"{theme}-{name}.png"
                page.screenshot(path=str(path))
                print(f"  {path}")
                ctx.close()
        browser.close()


if __name__ == "__main__":
    main()
