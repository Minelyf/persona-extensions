"""The backend on LarkAgentX. LarkAgentX has no license, so the package does not carry its code: on first start it is
downloaded from GitHub at the commit in upstream.json and checked file by file, and its dependencies are installed from
the hash-locked requirements.txt. Nothing here imports it at the top: the handshake must not wait for either."""
import asyncio
import hashlib
import io
import json
import os
import shutil
import subprocess
import sys
import tarfile
import time
import urllib.request
import zlib
from pathlib import Path

UPSTREAM = os.path.join(os.path.dirname(os.path.abspath(__file__)), "upstream.json")
FETCH_SECONDS = 120
ARCHIVE_MOST = 64 << 20
PIP_SECONDS = 300
PYPI_INDEX = "https://pypi.org/simple"
# A connection that lasted this long was fine: the next failure starts the backoff over.
STEADY_SECONDS = 60
BACKOFF_MOST = 60


class Expired(Exception):
    """The session is over: the user has to scan again. Not being able to reach Feishu is any other exception."""


def _fields(raw):
    """The length-delimited fields of a protobuf message, the first of each by number, as bytes. Enough to read the
    rich text's few string properties without generated classes; anything malformed ends the read."""
    out, i, n = {}, 0, len(raw)

    def varint():
        nonlocal i
        shift = value = 0
        while i < n:
            b = raw[i]
            i += 1
            value |= (b & 0x7F) << shift
            if not b & 0x80:
                return value
            shift += 7
        raise ValueError("truncated varint")

    try:
        while i < n:
            key = varint()
            number, wire = key >> 3, key & 7
            if wire == 0:
                varint()
            elif wire == 1:
                i += 8
            elif wire == 5:
                i += 4
            elif wire == 2:
                size = varint()
                out[number] = raw[i:i + size]  # the last one wins, as protobuf reads a repeated scalar
                i += size
            else:
                break
    except ValueError:
        pass
    return out


def _string(raw, number):
    return (_fields(raw).get(number) or b"").decode("utf-8", "replace")


# LarkAgentX's rich text: its tags (RichTextElement.Tag) and what it writes for the ones with no text.
_TEXT_TAGS = (1, 3, 7, 8, 9, 17)  # TEXT, P, B, I, U, LINK: TextProperty.content (LINK: LinkProperty.url) is field 1
_AT, _ANCHOR, _PARAGRAPH = 5, 6, 3
_PLACEHOLDERS = {2: "[图片]", 5: "[@]", 10: "[表情]", 18: "[视频]", 22: "[文档]"}


def rich_text(rich):
    """A TEXT or POST message's rich text as LarkAgentX reads it (`extract_rich_text`), but with its links. LarkAgentX
    drops an A element whole, its title and its address: a pasted Feishu doc link reached Persona as the words around
    it (2026-11-12). A link is its title, a space and its address (the address alone when the title is it or empty).
    None when the message has no link, so LarkAgentX's own text stays (and so does an empty result)."""
    try:
        dictionary = rich["elements"]["dictionary"]
    except (KeyError, TypeError):
        return None
    if not any(isinstance(v, dict) and v.get("tag") == _ANCHOR for v in dictionary.values()):
        return None
    ids = rich.get("elementIds") or []
    if ids:
        ordered = [dictionary[k] for k in ids if k in dictionary] + [v for k, v in dictionary.items() if k not in set(ids)]
    else:
        try:
            ordered = [v for _, v in sorted(dictionary.items(), key=lambda item: int(item[0]))]
        except ValueError:
            ordered = list(dictionary.values())
    text, after_link = "", False
    for v in ordered:
        tag, prop = v.get("tag", 1), v.get("property")
        if not prop:
            continue
        piece = None
        if tag in _TEXT_TAGS:
            piece = _string(prop, 1) + ("\n" if tag == _PARAGRAPH else "")
        elif tag == _AT:
            piece = _string(prop, 2) or None  # AtProperty.content
        elif tag == _ANCHOR:
            # AnchorProperty is not in LarkAgentX's proto.proto (only Tag.A = 6 and RichText.anchorIds = 7); its fields
            # are LarkAgentX's lark_all_pb2 entities.RichTextElement.AnchorProperty at the pinned commit: href 1,
            # content 2, textContent 3.
            # tests/test_real.py checks them there; check again when the pin moves.
            href, title = _string(prop, 1), _string(prop, 2) or _string(prop, 3)
            title = title.strip()
            piece = href if not title or title == href else ("%s %s" % (title, href) if href else title)
            if piece and text and not text[-1].isspace():
                piece = " " + piece
        if piece is None:
            piece = _PLACEHOLDERS.get(tag, "")
        if after_link and piece and not piece[0].isspace():
            piece = " " + piece  # an address glued to the next words is a different address
        text += piece
        after_link = tag == _ANCHOR and bool(piece)
    return text


def normalize(msg, extract_key):
    """A message of LarkAgentX's, live or from history, as the connector keeps it. Pure, for the tests."""
    msg_type = msg.get("msg_type") or 0
    name = msg.get("msg_type_name") or "UNKNOWN"
    data = msg.get("content_data") or {}
    key = extract_key(msg_type, data) if isinstance(data, dict) else ""
    text = msg.get("content") or ""
    if name in ("TEXT", "POST") and isinstance(data, dict):
        linked = rich_text(data.get("richText"))
        if linked:  # an empty rebuild keeps LarkAgentX's text, as its own `extract_rich_text(...) or text` does
            text = ("[富文本] %s\n%s" % (data.get("title", ""), linked)).strip() if name == "POST" else linked
    media = []
    if name in ("IMAGE", "STICKER") and key:
        media = [{"kind": name.lower(), "key": key}]
    elif name == "FILE":
        media = [{"kind": "file", "name": data.get("name") or ""}]
    elif name == "MEDIA":
        media = [{"kind": "video", "name": data.get("name") or ""}]
    elif name == "AUDIO":
        media = [{"kind": "voice"}]
    at = float(msg.get("create_time") or 0)
    if at > 1e12:
        at /= 1000
    root = msg.get("root_id") or ""
    return {
        "id": str(msg.get("msg_id") or ""), "chat": str(msg.get("chat_id") or ""),
        "chat_kind": "dm" if msg.get("chat_type") == 1 else "group",
        "position": int(msg.get("position") or 0), "sender": str(msg.get("from_id") or ""), "type": name,
        "text": text, "at": at or time.time(), "root": "" if root == "0" else root,
        "parent": msg.get("parent_id") or "", "at_me": bool(msg.get("at_me")), "media": media,
    }


def fetch_upstream(lock, data_dir, urlopen=urllib.request.urlopen):
    """LarkAgentX at lock's commit, in data_dir: the directory to put on sys.path. Downloaded once from lock's archive;
    only the files lock names are kept, each with its SHA-256, and any one missing or different fails the whole."""
    target = os.path.join(data_dir, "larkagentx-" + lock["commit"][:12])
    if os.path.isdir(target):
        return target
    try:
        with urlopen(lock["archive"], timeout=FETCH_SECONDS) as r:
            data = r.read(ARCHIVE_MOST + 1)
    except OSError as e:
        raise RuntimeError("下载不了 LarkAgentX：%s" % type(e).__name__) from e
    if len(data) > ARCHIVE_MOST:
        raise RuntimeError("下载的 LarkAgentX 太大了")
    want, got = lock["files"], {}
    try:
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
            for member in archive:
                # The archive's top directory is <repository>-<commit>: paths are taken below it.
                rel = member.name.split("/", 1)[-1] if "/" in member.name else ""
                if not member.isfile() or rel not in want:
                    continue
                body = archive.extractfile(member).read()
                if hashlib.sha256(body).hexdigest() != want[rel]:
                    raise RuntimeError("下载的 LarkAgentX 和锁定的不一样：" + rel)
                got[rel] = body
    except (tarfile.TarError, EOFError, OSError, zlib.error) as e:
        raise RuntimeError("下载的 LarkAgentX 不是完整的压缩包") from e
    missing = sorted(set(want) - set(got))
    if missing:
        raise RuntimeError("下载的 LarkAgentX 少了文件：" + missing[0])
    building = target + ".new"
    shutil.rmtree(building, ignore_errors=True)
    # Written under the names lock gives, never a name from the archive.
    for rel in sorted(want):
        path = os.path.join(building, *rel.split("/"))
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(got[rel])
    os.rename(building, target)
    drop_others(data_dir, "larkagentx-", target)
    return target


def drop_others(data_dir, prefix, keep):
    """Removes what earlier versions left under prefix in data_dir, now that keep is in place."""
    for name in os.listdir(data_dir):
        path = os.path.join(data_dir, name)
        if name.startswith(prefix) and path != keep and os.path.isdir(path):
            shutil.rmtree(path, ignore_errors=True)


class RealBackend:
    Expired = Expired

    def __init__(self, root, data_dir, region):
        self._root = root
        self._data_dir = data_dir
        self._region = region
        self._client = None
        self._auth = None
        self._qr = None
        self._save = None
        self._extract_key = None
        self._lark = None

    def prepare(self):
        """Makes LarkAgentX importable: its code and its dependencies, both in the data directory."""
        if sys.version_info < (3, 9):
            raise RuntimeError("LarkAgentX 要 Python 3.9 以上")
        with open(UPSTREAM, encoding="utf-8") as f:
            upstream = fetch_upstream(json.load(f), self._data_dir)
        deps = self._deps()
        sys.path[:0] = [upstream, deps]
        # Read when LarkAgentX is imported: its own files stay in the plugin's data directory, on the chosen site.
        os.environ["LARKX_HOME"] = os.path.join(self._data_dir, "larkx")
        os.environ["LARKX_REGION"] = self._region
        from loguru import logger
        # Its log carries bodies and addresses: keep where a problem happened and leave the message out.
        logger.remove()
        logger.add(sys.stderr, level="ERROR", format="larkx: {level} {name}:{function}:{line}")
        import larkx
        from larkx import media
        self._lark = larkx
        self._extract_key = media.extract_resource_key

    def _deps(self):
        requirements = os.path.join(self._root, "requirements.txt")
        with open(requirements, "rb") as f:
            digest = hashlib.sha256(f.read()).hexdigest()[:12]
        deps = os.path.join(self._data_dir, "deps-py%d%d-%s" % (sys.version_info[0], sys.version_info[1], digest))
        if os.path.isdir(deps):
            return deps
        building = deps + ".new"
        shutil.rmtree(building, ignore_errors=True)
        # PyPI itself, whatever index the machine's pip config names: the manifest's network list has only PyPI's hosts,
        # and in the hub's sandbox (or behind its egress proxy) a mirror is refused. Every file is checked against the
        # hashes in requirements.txt, the dependencies' dependencies too.
        done = subprocess.run([sys.executable, "-m", "pip", "install", "--quiet", "--disable-pip-version-check", "--no-input",
                               "--index-url", PYPI_INDEX, "--require-hashes", "--target", building, "-r", requirements],
                              stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=PIP_SECONDS)
        if done.returncode != 0:
            tail = done.stderr.decode(errors="replace").strip().splitlines()[-1:]
            raise RuntimeError("pip 没装上依赖：" + (tail[0][:160] if tail else "没有输出"))
        os.rename(building, deps)
        drop_others(self._data_dir, "deps-", deps)
        return deps

    def _hub_auth(self, saved):
        backend = self

        class HubAuth(self._lark.LarkAuth):
            """LarkAuth with its credentials in the hub's encrypted store instead of a file."""

            def load(self):
                from larkx.region import get_region
                data = saved or {}
                if data.get("region"):
                    self.region = get_region(data["region"])
                self.cookies = data.get("cookies") or {}
                self.saved_at = data.get("saved_at", 0)
                self.device_id = data.get("device_id", "")
                self.app_key = data.get("app_key", "")
                self.user_id = data.get("user_id", "")
                self.csrf_token = data.get("csrf_token", "")
                self.web_app_id = data.get("web_app_id", "")
                return bool(self.cookies)

            def save(self):
                self.saved_at = time.time()
                backend._persist()

        return HubAuth(Path(os.devnull))

    def _persist(self):
        if self._save is not None:
            self._save(self._credentials())

    def _credentials(self):
        a = self._auth
        return {"region": a.region.name, "cookies": a.cookies, "saved_at": a.saved_at, "device_id": a.device_id, "app_key": a.app_key,
                "user_id": a.user_id, "csrf_token": a.csrf_token, "web_app_id": a.web_app_id}

    def on_credentials(self, save):
        """save is called with the credentials whenever LarkAgentX refreshes them."""
        self._save = save

    def _open(self):
        """The client for self._auth, and who it is."""
        self._client = self._lark.LarkClient(self._auth)
        user = str(self._auth.user_id)
        return {"id": user, "name": "%s（用户 …%s）" % (self._auth.region.label, user[-4:])}

    def restore(self, saved):
        self._auth = self._hub_auth(saved)
        try:
            return self._open()
        except self._lark.AuthExpired as e:
            # LarkAgentX says expired for a probe that never reached the server too.
            if "网络异常" in str(e):
                raise ConnectionError("飞书连不上") from e
            raise Expired() from e

    def login_start(self):
        from larkx.region import get_region
        self._qr = self._lark.QrLogin(get_region(self._region))
        return self._qr.qr_content

    def login_qr(self):
        return self._qr.qr_content

    def login_poll(self):
        """One look at the scan: waiting, done or expired (scanned away, cancelled, timed out)."""
        if self._qr is None:
            return "expired"
        finished, data = self._qr.wait(timeout=0.01, interval=0)
        if finished:
            return "done"
        return "expired" if data is not None else "waiting"

    def login_finish(self):
        self._auth = self._hub_auth(None)
        try:
            self._qr.finish(self._auth)
        except self._lark.AuthExpired as e:
            raise Expired() from e
        self._qr = None
        account = self._open()
        return account, self._credentials()

    def logout(self):
        self._client = None
        self._auth = None
        self._qr = None
        self._save = None

    def listen(self, on_message, on_link, stop):
        """Holds the long connection until stop, reconnecting with a growing wait."""
        wait = 1

        async def received(msg):
            await asyncio.to_thread(on_message, normalize(msg, self._extract_key))

        while not stop.is_set():
            client, began = self._client, time.time()
            if client is None:
                return
            try:
                on_link("up")
                self._run(client, received, stop)
            except self._lark.AuthExpired:
                on_link("expired")
                return
            except Exception:
                pass
            on_link("down")
            if time.time() - began > STEADY_SECONDS:
                wait = 1
            stop.wait(wait)
            wait = min(wait * 2, BACKOFF_MOST)

    @staticmethod
    def _run(client, received, stop):
        async def until_stopped():
            task = asyncio.ensure_future(client.connect_websocket(received))
            while not task.done():
                if stop.is_set():
                    task.cancel()
                    return
                await asyncio.sleep(1)
            task.result()

        asyncio.run(until_stopped())

    def send(self, chat, text, root):
        return self._client.send_msg(text, chat, root_id=root)

    def create_chat(self, user):
        chat = self._client.create_chat(user)
        if not chat:
            raise RuntimeError("建不了和这个人的私聊")
        return str(chat)

    def search(self, query):
        return self._client.search(query)

    def history(self, chat, positions):
        return [normalize(m, self._extract_key) for m in self._client.pull_history(chat, positions)]

    def chat_info(self, chat):
        info = self._client.get_chat_info(chat) or {}
        return {"name": info.get("name") or "", "last_position": int(info.get("lastMessagePosition") or 0)}

    def user_name(self, user, chat):
        return self._client.get_user_name(user, chat)

    def download(self, msg_id, chat, key, most):
        from larkx import media
        url = media.message_resource_url(msg_id, key, chat, host=self._auth.region.file_host)
        data, mime = media.download(self._auth, url, max_bytes=most)
        return mime or "application/octet-stream", data

