#!/usr/bin/env python3
"""scripts/global_region.py — 国际版注册地区自动完善（login.sh 集成）。

逆向自注册完善页面 chunk `RegisterRegion-CWhKzb4k.js`（web，codebuddy.ai/login）：

  - POST /billing/area/get-country-code  {filterForbidden:1}  → 可取国家列表
        （响应 data 是 JSON 字符串，内含 {"code":0,"data":{"list":[{EnName,Name,IOS2,IOS3,Code},...]}}）
  - POST /billing/area/get-user-area-info {action:"getUserAreaInfo"} → 检测当前地区（data 为 JSON 字符串，含 IOS2/enName）
  - POST /console/login/account {"attributes":{countryCode:[Code], countryFullName:[EnName], countryName:[IOS2]}}
        → 提交地区（实测幂等，Bearer 必需，成功 code:0）
  - GET  /auth/realms/copilot/overseas/user/register?userId=<uid> → 注册激活（code:200 成功；code:500 "region required" 需补地区）
  - POST /billing/ide/trial → 一次性 trial 加油包（幂等码 14051）

菜单展示遵循 web 国际版的白名单（HK/MO/SG/TH/PH/MY/ID）；拉取失败回退全量列表。

设计：login.sh 通过 heredoc import 本模块使用，token 经 shell 变量内插注入（不落 argv）。
交互读取用 /dev/tty 兜底——heredoc 下 python 的 stdin 是脚本本身，直接 input() 取的是管道。
"""
import json, os, sys, urllib.request, urllib.error

_BASE = "https://www.workbuddy.ai"

UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

# 国际版 web 白名单（RegisterRegion 内 K）；提交选择时作为默认展示集。
# 顺序 = web 端 ae 数组的展示顺序（HK, MO, SG, TH, PH, MY, ID）。
INL_CODES = ["HK", "MO", "SG", "TH", "PH", "MY", "ID"]


def _headers(token=None, extra=None):
    h = {
        "User-Agent": UA,
        "Accept": "application/json, text/plain, */*",
        "Content-Type": "application/json",
        "Origin": _BASE,
        "Referer": _BASE + "/",
    }
    if token:
        h["Authorization"] = "Bearer " + token
    if extra:
        h.update(extra)
    return h


def _post(base, path, token=None, body=None, extra=None, timeout=20):
    if base is None:
        base = _BASE
    if body is None:
        body = {}
    data = json.dumps(body).encode()
    req = urllib.request.Request(base + path, method="POST", headers=_headers(token, extra), data=data)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode() or "{}")


def _get(base, path_with_qs, token=None, extra=None, timeout=20):
    if base is None:
        base = _BASE
    req = urllib.request.Request(base + path_with_qs, method="GET", headers=_headers(token, extra))
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode() or "{}")


def fetch_countries(base=None, intl_only=True):
    """拉取可选地区列表。返回 (ok, list, msg)。

    list 元素为 dict：{EnName, Name, IOS2, IOS3, Code}。intl_only=True 时按
    国际版 web 白名单过滤（web 端 intl 只展示 HK/MO/SG/TH/PH/MY/ID）。
    """
    try:
        outer = _post(base, "/billing/area/get-country-code", body={"filterForbidden": 1})
        if outer.get("code") != 0:
            return False, [], outer.get("msg", f"code={outer.get('code')}")
        raw = outer.get("data")
        inner = json.loads(raw) if isinstance(raw, str) else raw or {}
        all_lst = (inner.get("data") or {}).get("list") or []
        if intl_only:
            inl_map = {c["IOS2"]: c for c in all_lst if c.get("IOS2") in INL_CODES}
            inl = [inl_map[c] for c in INL_CODES if c in inl_map]
            return True, inl, "ok"
        return True, all_lst, "ok"
    except Exception as e:
        return False, [], str(e)


def detect_region(token, base=None):
    """检测当前注册地区。返回 (ok, ios2, enName, msg)。本模块的『检测』消费端。"""
    try:
        outer = _post(base, "/billing/area/get-user-area-info", token, {"action": "getUserAreaInfo"})
        if outer.get("code") != 0:
            return False, "", "", outer.get("msg", f"code={outer.get('code')}")
        raw = outer.get("data")
        inner = json.loads(raw) if isinstance(raw, str) else raw or {}
        data = inner.get("data") or {}
        return True, data.get("IOS2", ""), data.get("enName", ""), "ok"
    except Exception as e:
        return False, "", "", str(e)


def submit_region(token, country, base=None):
    """POST /console/login/account 提交注册地区。返回 (ok, msg)。"""
    try:
        attrs = {
            "countryCode": [str(country["Code"])],
            "countryFullName": [str(country["EnName"])],
            "countryName": [str(country["IOS2"])],
        }
        body = _post(base, "/console/login/account", token, {"attributes": attrs})
        if body.get("code") == 0:
            return True, "ok"
        return False, body.get("msg", f"code={body.get('code')}")
    except Exception as e:
        return False, str(e)


def activate_region(token, uid, base=None):
    """GET register 激活/查询。返回 (ok, needs_region, msg)。

    code:200 → (True, False, "register success")；code:500 或 msg 含 region required →
    (False, True, msg)；网络错误 → (False, False, str)。携带 X-User-Id 与官方 web 对齐。
    """
    try:
        body = _get(base, "/auth/realms/copilot/overseas/user/register?userId=" + uid, token,
                    extra={"X-User-Id": uid})
        code = body.get("code")
        msg = body.get("msg", "")
        if code == 200:
            return True, False, "register success"
        if code == 500 or "region required" in msg.lower():
            return False, True, msg or f"code={code}"
        return False, False, msg or f"code={code}"
    except Exception as e:
        return False, False, str(e)


def trial(token, base=None):
    """POST /billing/ide/trial 领取一次性加油包。返回 (ok, already, msg)。

    幂等码 14051（200 或 4xx 两种形态）→ (True, True, ...)。
    """
    try:
        body = _post(base, "/billing/ide/trial", token, {})
        if body.get("code") == 0:
            return True, False, "ok"
        if "14051" in json.dumps(body, ensure_ascii=False):
            return True, True, "已领取"
        return False, False, body.get("msg", json.dumps(body, ensure_ascii=False)[:150])
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace") or "{}"
        try:
            body = json.loads(raw)
        except (json.JSONDecodeError, ValueError):
            # 非 JSON 4xx（网关/WAF 的纯文本页）：按 idempotent 文本与 HTTP 码处理。
            return False, "14051" in raw, f"http {e.code}: {raw[:120]}"
        if "14051" in json.dumps(body, ensure_ascii=False):
            return True, True, "已领取"
        return False, False, body.get("msg", f"http {e.code}")
    except Exception as e:
        return False, False, str(e)


def complete_flow(token, uid, pick=None, call_register=True, base=None):
    """整体完善流程：先 register → 需补地区则用 pick 提交 → 重新 register 验证。

    pick 为国家 dict（来自 fetch_countries）。call_register=False 时跳过前置
    register 检查（登录脚本已先调 activate_region 判定需补地区后直接提交）。
    返回 (ok, msg)。
    """
    if call_register:
        ok, needs, msg = activate_region(token, uid, base)
        if ok:
            return True, "register success"
        if not needs:
            return False, f"register 失败: {msg}"
        # needs region → 需提交
    if pick is None:
        return False, "需完善注册地区，但未提供选择"
    sok, smsg = submit_region(token, pick, base)
    if not sok:
        return False, f"提交地区失败: {smsg}"
    # 重新 register 验证
    ok2, needs2, msg2 = activate_region(token, uid, base)
    if not ok2:
        return False, f"提交地区后 register 仍失败: {msg2} (needs_region={needs2})"
    return True, "地区已完善，register 成功"


def _read_line(prompt):
    """交互读取：heredoc 下 stdin 是管道，回退 /dev/tty。非交互环境返回 ''。"""
    if sys.stdin.isatty():
        return input(prompt)
    try:
        with open("/dev/tty", "r") as t:
            sys.stdout.write(prompt)
            sys.stdout.flush()
            return t.readline().strip()
    except OSError:
        return ""


def interactive_menu(countries, detected_ios2="", title="国际版账号需完善注册地区"):
    """终端编号菜单。返回选中的国家 dict；取消/无输入返回 None。"""
    if not countries:
        print("（无可选地区）")
        return None
    print(f"\n{title}（请输入编号选择注册地区）：")
    for i, c in enumerate(countries, 1):
        mark = "  ◀ 检测到当前地区" if c["IOS2"] == detected_ios2 else ""
        print(f"{i:>3}) {c['IOS2']:<4} {c['Code']:<8} {c['EnName']}{mark}")
    try:
        raw = _read_line("\n请选择注册地区编号（回车取消）: ")
        raw = raw.strip()
        if not raw:
            return None
        idx = int(raw)
        if 1 <= idx <= len(countries):
            return countries[idx - 1]
        print(f"无效编号 {raw}")
        return None
    except (ValueError, EOFError):
        return None


def main(argv=None):
    """独立 CLI（供手测/调试）：WORKBUDDY_TOKEN env 传入 token，避免 argv 泄露。"""
    import argparse
    p = argparse.ArgumentParser(description="国际版注册地区自动完善（调试）")
    p.add_argument("--uid", required=True)
    p.add_argument("--auto", help="直接指定 IOS2（如 SG），跳过交互菜单")
    a = p.parse_args(argv)
    token = os.environ.get("WORKBUDDY_TOKEN") or ""
    if not token:
        print("需要 WORKBUDDY_TOKEN 环境变量", file=sys.stderr)
        return 1
    ok, needs, msg = activate_region(token, a.uid)
    if ok:
        print("register: 成功")
        return 0
    if not needs:
        print(f"register: 失败 {msg}")
        return 1
    okc, countries, msgc = fetch_countries()
    if not okc:
        print(f"拉取地区列表失败: {msgc}")
        return 1
    pick = None
    if a.auto:
        pick = next((c for c in countries if c["IOS2"] == a.auto), None)
        if pick is None:
            # 下拉取全量再找
            _, allc, _ = fetch_countries(intl_only=False)
            pick = next((c for c in allc if c["IOS2"] == a.auto), None)
        if pick is None:
            print(f"未找到 {a.auto}")
            return 1
    else:
        _, ios2, _, _ = detect_region(token)
        pick = interactive_menu(countries, ios2)
        if pick is None:
            print("已取消")
            return 1
    ok, msg = complete_flow(token, a.uid, pick=pick, call_register=True)
    print("complete:", msg)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())