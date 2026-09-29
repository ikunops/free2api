#!/usr/bin/env python3
"""school_open_day_2026 —— WorkBuddy/CodeBuddy 小程序「开学季活动」任务自动化脚本。

用途：盘点/点亮/领奖/抽奖——读取任务中心与抽奖余额、上报非人工任务的完成
判据、领取奖励、清空幸运大转盘抽奖次数。默认只读，--run / --lottery-only
配合 --yes 才发写请求。

用法示例：
  python3 school_open_day_2026.py --list                  # 只读盘点全部账号
  python3 school_open_day_2026.py --list <uid8> [...更多uid]  # 盘点指定账号
  python3 school_open_day_2026.py --run --yes             # 点亮并领取任务
  python3 school_open_day_2026.py --lottery-only --yes    # 只抽奖（清空抽奖次数）
  账号参数为 auths/workbuddy-<uid8>.json 的 uid 前缀，可多个；缺省或 ALL 即全部。

参数：
  accounts        uid 前缀（可多个）或 ALL；--token 时可不传
  --list          只读盘点（默认行为）
  --run           执行点亮/领取动作（需 --yes 放行写）
  --lottery-only  只抽奖不做任务（任务已全领时的日常补抽）
  --yes           放行写操作（配合 --run / --lottery-only）
  --token         显式覆盖 token（不同源凭证逃生门），可用 --uid 指定日志 uid
  --gap           写动作间隔秒数（默认 1.5，最小 1.0）

风险提示：
  - 活动接口/任务集为服务端下发，随时可能变更；脚本按字段寻址，未知 task_code
    保守跳过，不保证一定点亮成功。
  - 完成判据上报 / viewed / claim / wheel/draw 均属写操作，可能触发频控/风控，
    写动作间隔默认 ≥1s。
  - claim 会给账号加抽奖机会、draw 会真实产生积分/实物券收益，勿频繁重复；
    余额为 0 或服务端异常即停，不过度抽。
  - student-verify（学生认证）为人工环节，脚本不碰、不伪造。

依赖：复用 scripts/task_common.py 的 load_auth / AUTHS 常量；
账号凭证位于 auths/ 目录下 workbuddy-<uid8>.json。
"""
import sys, os, json, time, hashlib, argparse, glob, uuid, urllib.request, urllib.error

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc  # 仅复用 load_auth / AUTHS 常量，不复用其 _headers（头不同源）

# --------------------------------------------------------------------------
# 端点常量（接口地址）
# --------------------------------------------------------------------------
CLOUD_AGENT = "https://www.codebuddy.cn"          # getCloudAgentEndpoint()
COPILOT     = "https://copilot.tencent.com"       # growth/chat 域（desktop 上报用）
SCHOOL      = CLOUD_AGENT + "/portal/activity/school"
FRESHMAN    = CLOUD_AGENT + "/portal/activity/freshman"
TEACHER     = CLOUD_AGENT + "/portal/activity/teacher"

# school 开学季活动 code（事件 activityId 字段值）
ACTIVITY_ID = "school_open_day_2026"

# 小程序由微信注入 UA；桌面上报用桌面 UA
MP_UA = ("Mozilla/5.0 (Linux; Android 14; MicroMessenger/8.0.49 WeChat/0.8.0 "
         "MiniProgramEnv/android; wkbrowser xweb)")
DESKTOP_UA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"

# 任务分类表（task_code -> 处理方式）。mode 语义：
#   manual  人工环节（学生认证/验证码/审核），--run 一律跳过
#   share   share-complete HTTP 端点点亮
#   report  /v2/report 事件上报点亮（mini chat / 桌面指纹 6 连 / 专家事件）
# 未在表内（服务端新增任务）保守按 unknown 跳过。
KNOWN_TASKS = {
    "task_student_verify": {"mode": "manual",  "note": "微信学生认证（人工，不碰）"},
    "share_invite":        {"mode": "share",   "note": "分享活动给好友；share-complete 点亮"},
    "chat_3_times":        {"mode": "report",  "note": "与 AI 对话 3 次；mini chat_request_send+activityId 点亮",
                            "report_kind": "mini_chat"},
    "desktop_chat_1_time": {"mode": "report",  "note": "桌面端对话 1 次；桌面指纹 6 连+activityId 点亮",
                            "report_kind": "desktop_seq"},
    "expert_use":          {"mode": "report",  "note": "召唤开学季专家并对话；BackToSchool 分类专家 + expert_actual_use 点亮",
                            "report_kind": "expert"},
}

# 活动接口成功判据：返回 code===0 视为成功
OK_CODES = (0,)

# 抽奖转盘奖品映射（/config prizes 返回的 prize_code -> 标签）：
# school_credit_6=6积分 / school_credit_66=66积分，school_voucher_* 为实物券
# （瑞幸/KFC/酷狗），credit_amount 为积分增量。
LOTTERY_PRIZE_LABELS = {
    "school_credit_6":      {"label": "6积分",   "type": "credit"},
    "school_credit_66":     {"label": "66积分",  "type": "credit"},
    "school_voucher_luckin": {"label": "瑞幸咖啡15元券", "type": "voucher"},
    "school_voucher_kfc_ok": {"label": "肯德基OK餐券",   "type": "voucher"},
    "school_voucher_kfc_ice": {"label": "肯德基冰淇淋券", "type": "voucher"},
    "school_voucher_kugou":  {"label": "酷狗会员月卡券",  "type": "voucher"},
}
# 未知 prize_code 显示原码，不编造内容


class AuthError(RuntimeError):
    """鉴权/未绑定微信类硬错误（不重试）。"""


class TransientError(RuntimeError):
    """网络/5xx 可重试错误。"""


def derive_id(auth, salt):
    """由 uid 稳定派生一个 36 位 hex 设备标识（桌面指纹 machineId 用，幂等）。"""
    return hashlib.md5(f"{salt}:{auth['uid']}".encode()).hexdigest()[:36]


def mask(token):
    """脱敏 token，仅留首 8 位。"""
    return (token[:8] + "...") if token else "(none)"


def _build_headers(token, extra=None):
    """组装小程序域请求头（Authorization Bearer + 可选 extra 覆盖）。"""
    h = {
        "Authorization": "Bearer " + token,
        "Accept": "application/json",
        "Content-Type": "application/json",
        "User-Agent": MP_UA,
    }
    if extra:
        h.update(extra)
    return h


def _parse_response(status, raw_bytes):
    try:
        return status, json.loads(raw_bytes.decode("utf-8", "replace"))
    except Exception:
        return status, {"raw": raw_bytes.decode("utf-8", "replace")[:300]}


def _request(token, method, url, body=None, headers=None, timeout=30):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, headers=_build_headers(token, headers),
                                 method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return _parse_response(r.status, r.read())
    except urllib.error.HTTPError as e:
        return _parse_response(e.code, e.read())
    except urllib.error.URLError as e:
        raise TransientError(repr(e.reason))
    except Exception as e:
        raise TransientError(repr(e))


def request_retry(token, method, url, body=None, headers=None, retries=3, gap=1.0):
    last = None
    for i in range(1, retries + 1):
        try:
            st, r = _request(token, method, url, body, headers)
        except TransientError as e:
            last = e
            print(f"  [warn] 网络错误重试 {i}/{retries}: {e}")
            if i < retries:
                time.sleep(gap * i)
            continue
        if 500 <= st < 600 and i < retries:
            last = TransientError(f"http {st}")
            print(f"  [warn] 5xx={st} 重试 {i}/{retries}")
            time.sleep(gap * i)
            continue
        return st, r
    if last is not None:
        raise last
    return st, r


def _interpret_failure(st, r):
    code = r.get("code") if isinstance(r, dict) else None
    msg = (r.get("message") or r.get("msg") or (r.get("raw") if r else "") or "") \
        if isinstance(r, dict) else str(r)
    if st == 401 or code in ("401", 401, "40100", 40100):
        return f"401/40100 未绑微信或 token 失效（小程序会静默刷新，本脚本不刷新）", True
    if st == 403:
        return "403 forbidden", True
    if st == 404:
        return "404 路由不存在", False
    if code in ("41000", 41000):
        return "41000 活动未开始或已结束", False
    if code in ("40901", 40901):
        return "40901 任务暂不可领取（未完成或已领）", False
    if code in ("83400", 83400):
        return "83400 验证码已过期", False
    if code in ("50300", 50300):
        return "50300 学生认证服务暂不可用", False
    return f"http={st} code={code} {str(msg)[:120]}", False


# --------------------------------------------------------------------------
# 只读盘点
# --------------------------------------------------------------------------
def fetch_school_tasks(token):
    """GET /portal/activity/school/tasks -> (tasks, in_period)。只读。"""
    st, r = request_retry(token, "GET", SCHOOL + "/tasks")
    if st != 200 or (isinstance(r, dict) and r.get("code") not in OK_CODES):
        msg, is_auth = _interpret_failure(st, r)
        raise (AuthError if is_auth else RuntimeError)(f"school/tasks {msg}")
    data = (r or {}).get("data") or {}
    return data.get("tasks") or [], bool(data.get("in_period"))


def fetch_profile(token, which):
    base = FRESHMAN if which == "freshman" else TEACHER
    st, r = request_retry(token, "GET", base + "/profile")
    if st != 200 or (isinstance(r, dict) and r.get("code") not in OK_CODES):
        msg, is_auth = _interpret_failure(st, r)
        raise (AuthError if is_auth else RuntimeError)(f"{which}/profile {msg}")
    return (r or {}).get("data") or None


def list_account(auth, stats):
    """只读盘点单个账号。"""
    uid8 = (auth.get("uid") or "")[:8] or "?"
    print(f"== {uid8} ({auth.get('nick') or '-'}) ==")
    try:
        tasks, in_period = fetch_school_tasks(auth["token"])
    except (AuthError, RuntimeError) as e:
        print(f"[school2026] {uid8} school/tasks 拉取失败: {e}")
        stats["fail"] += 1
        tasks, in_period = [], False

    print(f"[school2026] {uid8} school/tasks in_period={in_period} tasks={len(tasks)}")
    for t in tasks:
        code = t.get("task_code") or "?"
        status = t.get("status") or "?"
        prog = t.get("progress") or 0
        target = t.get("target_count") or 1
        kinds = {"single": "单次", "recurring": "每日"}.get(t.get("task_type"), t.get("task_type"))
        spec = KNOWN_TASKS.get(code, {})
        mode = spec.get("mode", "unknown")
        reward = t.get("reward_credit") or 0
        title = t.get("title") or ""
        print(f"  - {code:<20} status={status:<10} {prog}/{target} reward={reward:>3} "
              f"[{kinds}/{mode}] {title}")
        desc = (t.get("description") or "").strip()
        if desc:
            print(f"      desc: {desc}")
        if spec.get("note"):
            print(f"      note: {spec['note']}")
    if not tasks:
        print("  (无任务 / 未登录或活动未开启)")

    for which in ("freshman", "teacher"):
        try:
            d = fetch_profile(auth["token"], which)
        except (AuthError, RuntimeError) as e:
            print(f"[school2026] {uid8} {which}/profile 拉取失败: {e}")
            stats["fail"] += 1
            continue
        if d is None:
            print(f"[school2026] {uid8} {which}/profile -> 空")
            continue
        print(f"[school2026] {uid8} {which}/profile claimed={d.get('claimed')} "
              f"enabled={d.get('enabled')}")
    stats["accounts"] += 1


# --------------------------------------------------------------------------
# school 专属 HTTP 端点（点亮动作）
# --------------------------------------------------------------------------
def post_viewed(token, code):
    """POST /tasks/{code}/viewed —— 「接任务」动作（pending -> in_progress）。"""
    st, r = request_retry(token, "POST", f"{SCHOOL}/tasks/{code}/viewed")
    if st != 200 or (isinstance(r, dict) and r.get("code") not in OK_CODES):
        msg, is_auth = _interpret_failure(st, r)
        raise (AuthError if is_auth else RuntimeError)(f"viewed {code} {msg}")
    return st, r


def post_share_complete(token):
    """POST /tasks/share-complete {channel:"wechat"} —— share_invite 完成判据。"""
    st, r = request_retry(token, "POST", f"{SCHOOL}/tasks/share-complete",
                          {"channel": "wechat"})
    if st != 200 or (isinstance(r, dict) and r.get("code") not in OK_CODES):
        msg, is_auth = _interpret_failure(st, r)
        raise (AuthError if is_auth else RuntimeError)(f"share-complete {msg}")
    return st, r


def post_claim(token, code):
    """POST /tasks/{code}/claim —— 领奖（completed -> claimed），发抽奖机会。"""
    st, r = request_retry(token, "POST", f"{SCHOOL}/tasks/{code}/claim")
    if st != 200 or (isinstance(r, dict) and r.get("code") not in OK_CODES):
        msg, is_auth = _interpret_failure(st, r)
        raise (AuthError if is_auth else RuntimeError)(f"claim {code} {msg}")
    return st, r


# --------------------------------------------------------------------------
# 抽奖（幸运大转盘）
# --------------------------------------------------------------------------
def fetch_lottery_config(token):
    """GET /portal/activity/school/config —— 抽奖盘面 + 余额（只读）。

    返回 (config, chance)：
      config = data（含 start_at/end_at/in_period/prizes[]）
      chance = data.chance = {balance, total_earned, voucher_won, lottery_limit}；
      chance.balance 即抽奖次数余额。"""
    st, r = request_retry(token, "GET", SCHOOL + "/config")
    if st != 200 or (isinstance(r, dict) and r.get("code") not in OK_CODES):
        msg, is_auth = _interpret_failure(st, r)
        raise (AuthError if is_auth else RuntimeError)(f"config {msg}")
    data = (r or {}).get("data") or {}
    return data, (data.get("chance") or {})


def post_lottery_draw(token, draw_uuid):
    """POST /portal/activity/school/wheel/draw {draw_uuid} —— 抽一次转盘。

    draw_uuid 每轮一次性（随机 UUID），抽后即弃。
    返回 (st, resp)；resp.data={prize_code, credit_amount, chance_balance}。
    边界：余额 0 时返回 HTTP 409 code=40900 "no chance"（不再扣，安全）。
    """
    body = {"draw_uuid": draw_uuid}
    st, r = request_retry(token, "POST", f"{SCHOOL}/wheel/draw", body)
    return st, r


def lottery_prize_text(prize_code, credit_amount):
    """把 prize_code 映射成语义化文本；未知码直接原样返回，不编造。"""
    info = LOTTERY_PRIZE_LABELS.get(prize_code)
    if not info:
        return f"{prize_code}"
    if info["type"] == "credit":
        return f"{info['label']}（+{credit_amount} Credit）"
    return info["label"]


# --------------------------------------------------------------------------
# /v2/report 事件上报
# --------------------------------------------------------------------------
def report_events(auth, events, host=CLOUD_AGENT, extra_headers=None):
    """POST {host}/v2/report，body=[event,...]。返回 (http_status, resp_dict)。"""
    st, r = request_retry(auth["token"], "POST", host + "/v2/report", events,
                          headers=extra_headers)
    return st, r


# --------------------------------------------------------------------------
# school 开学季专家（BackToSchool 分类）——expert_use 判据载体
# --------------------------------------------------------------------------
SCHOOL_EXPERT_CATEGORY = "16-BackToSchool"   # 分类 id（带数字前缀，list API 不做归一化）
SCHOOL_EXPERT_FALLBACK = {                    # 若分类专家列表拉取失败，回落已知专家
    "ex_jB0dyFIQJEWa": {"name": "论小舟", "title": "论文写作导师"},
    "ex_lQjkerakvIex": {"name": "英语学习教练", "title": "大学英语学习教练"},
}


def fetch_school_expert(token):
    """拉取一个 BackToSchool 分类的真实开学季专家。

    端点：POST {cloudAgent}/v2/operation-platform/market/expert/list
      body {page:1,page_size:20,sort_by:"use_count",sort_order:"desc",
            categories:["16-BackToSchool"],expert_type:"agent",
            edition_mode:"all,domestic"}
    返回 (expert_id, display_name, profession) 元组；列表空/失败回落已知专家。"""
    body = {"edition_mode": "all,domestic", "page": 1, "page_size": 20,
            "sort_by": "use_count", "sort_order": "desc",
            "categories": [SCHOOL_EXPERT_CATEGORY], "expert_type": "agent"}
    try:
        st, r = request_retry(token, "POST", CLOUD_AGENT + "/v2/operation-platform/market/expert/list", body)
        if st != 200 or (isinstance(r, dict) and r.get("code") not in OK_CODES):
            raise RuntimeError(f"expert/list {r.get('code') if isinstance(r, dict) else r}")
        exps = ((r or {}).get("data", {}) or {}).get("experts") or []
        for e in exps:
            eid = e.get("expert_id")
            if not eid:
                continue
            _dn = e.get("display_name_zh") or {}
            dn = (_dn.get("zh") if isinstance(_dn, dict) else _dn) or eid
            _pf = e.get("profession_zh") or {}
            pf = (_pf.get("zh") if isinstance(_pf, dict) else _pf) or ""
            return eid, dn, pf
    except (AuthError, RuntimeError) as e:
        print(f"  [warn] 拉取 BackToSchool 专家失败，回落已知专家: {e}")
    for eid, info in SCHOOL_EXPERT_FALLBACK.items():
        return eid, info["name"], info["title"]
    raise RuntimeError("无可用开学季专家")


def expert_actual_use_event(auth, expert_id, expert_name, conversation_id):
    """expert_actual_use 事件（点亮 expert_use）。

    事件字段：id/name/expertTitle/type/characterCount/expertType + 公共字段 +
    必须 activityId + conversationId（服务端不校验真实性）。"""
    now = int(time.time() * 1000)
    return {
        "eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
        "source": "mini_program", "ideName": "wx_app_cloud", "ideType": "WorkBuddy_MP",
        "extName": "workbuddy-mp", "extVersion": "SaaS",
        "machineId": derive_id(auth, "machine"), "os": "android", "osVersion": "14",
        "arch": "arm64", "timezone": "Asia/Shanghai",
        "userId": auth["uid"], "userNickname": auth.get("nick", ""),
        "id": expert_id, "name": expert_id, "expertTitle": expert_name,
        "type": "send_message", "characterCount": 12, "expertType": "agent",
        "conversationId": conversation_id,
        "activityId": ACTIVITY_ID,
    }


def make_expert_event(auth):
    """构造一条 expert_actual_use 上报。返回 (events, host, extra_headers)。"""
    eid, name, _ = fetch_school_expert(auth["token"])
    conv = f"wbexp-{int(time.time() * 1000)}"
    return [expert_actual_use_event(auth, eid, name, conv)], CLOUD_AGENT, None


def mini_chat_event(auth, conversation_id):
    """小程序域 chat_request_send 事件（点亮 chat_3_times）。
    必须带 activityId=school_open_day_2026，否则服务端不关联任务。"""
    now = int(time.time() * 1000)
    return {
        "eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
        "source": "mini_program", "ideName": "wx_app_cloud", "ideType": "WorkBuddy_MP",
        "extName": "workbuddy-mp", "extVersion": "SaaS", "mode": "chat",
        "conversationId": conversation_id, "requestId": conversation_id,
        "inputLength": 12,
        "activityId": ACTIVITY_ID, "mentionContexts": [], "mentionContextCount": 0,
        "userId": auth["uid"],
    }


def desktop_fingerprint(auth):
    """桌面指纹：注入每个桌面事件。与 mini 形状的差异：
    ideName=WorkBuddy / extName=workbuddy-desktop / os=win32 / arch=x64 /
    osVersion=10.0.26220 / machineId/sessionId 稳定派生。"""
    now = int(time.time() * 1000)
    return {
        "timezone": "Asia/Shanghai", "reportDelay": 2000,
        "userId": auth["uid"], "username": auth.get("nick", ""),
        "userNickname": auth.get("nick", ""),
        "product": "SaaS", "releaseDate": 1789036585355,
        "commit": "5f9692923c93033111c51ad7b003eb80204a9b75",
        "ideName": "WorkBuddy", "ideType": "WorkBuddy", "ideVersion": "5.5.6",
        "machineId": derive_id(auth, "machine"), "sessionId": derive_id(auth, "session"),
        "extName": "workbuddy-desktop", "extVersion": "5.5.6",
        "os": "win32", "arch": "x64", "osVersion": "10.0.26220",
        "cpuCores": 20, "memorySize": 24,
        "timestamp": now, "presentAt": now,
    }


def desktop_chat_sequence(auth, conversation_id, request_id, message_id):
    """桌面端成功对话 6 连事件链：
    agent_task_created -> chat_message_send -> chat_request_send ->
    chat_message_response -> chat_message_status -> chat_request_response。
    必须整体上报（一次 6 连 = 一次桌面对话）+ activityId。"""
    now = int(time.time() * 1000)
    ev = []

    def mk(code, extra):
        e = {"eventCode": code}
        e.update(extra)
        ev.append(e)

    mk("agent_task_created", {
        "source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
        "requestModelId": "fast-model", "requestModelName": "fast-model",
        "has_repo": False, "repo_type": "none", "workspace_type": "empty",
        "has_connector": False, "connector_types": [],
        "has_mention": False, "mention_types": [],
        "has_template": False, "action": "", "template_name": "",
        "has_expert": False, "expert_id": "", "expert_name": "", "expert_industry_id": "",
        "has_skill": False, "skill_names": [],
        "conversationId": conversation_id, "messageId": message_id,
        "buddyId": "", "buddyName": "",
    })
    mk("chat_message_send", {
        "messageId": message_id + "-assistant", "historyCount": 0,
        "isContextTruncated": False, "currentStepCount": 1,
        "traceId": request_id, "rootRequestId": request_id,
        "parentConversationId": conversation_id,
        "agentName": "cli", "agentType": "main",
    })
    mk("chat_request_send", {
        "inputLength": 24, "isPlan": False, "isAutoExecuteTerminal": False,
        "isAutoModify": False, "codebaseEnable": False, "maxToken": 0,
        "maxSteps": 500, "temperature": 0, "maxRetries": 0,
        "mentionContexts": [], "knowledgeId": [], "knowledgeName": [],
        "codebaseId": "", "mentionContextCount": 0, "command": "",
        "recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
        "traceId": request_id, "rootRequestId": request_id,
        "parentConversationId": conversation_id,
        "agentName": "cli", "agentType": "main",
        "codebuddy.session_id": conversation_id,
        "codebuddy.conversation_request_id": request_id,
    })
    mk("chat_message_response", {
        "messageId": message_id + "-assistant", "responseModelId": "fast-model",
        "inputToken": 120, "outputToken": 80, "totalToken": 200,
        "cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
        "isSuccessful": True, "messageErrorCode": "", "finishReason": "stop",
        "firstTokenAt": now, "traceId": request_id,
        "conversationId": conversation_id,
        "rootRequestId": request_id, "parentConversationId": conversation_id,
        "agentName": "cli", "agentType": "main",
        "codebuddy.session_id": conversation_id,
        "codebuddy.conversation_request_id": request_id,
    })
    mk("chat_message_status", {
        "messageId": message_id + "-assistant", "messageErrorCode": "0",
        "traceId": request_id, "rootRequestId": request_id,
        "parentConversationId": conversation_id,
        "agentName": "cli", "agentType": "main",
    })
    mk("chat_request_response", {
        "mode": "craft", "toolCallCount": 0,
        "inputToken": 120, "outputToken": 80, "totalToken": 200,
        "cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
        "isSuccessful": True, "messageErrorCode": "", "finishReason": "stop",
        "rootRequestId": request_id, "parentConversationId": conversation_id,
    })
    return ev


def make_report_events(auth, kind):
    """按任务 kind 构造上报事件数组（含 activityId）。返回 (events, host)。"""
    now = int(time.time() * 1000)
    if kind == "mini_chat":
        return [mini_chat_event(auth, f"wbmp-{now}")], CLOUD_AGENT, None
    if kind == "expert":
        return make_expert_event(auth)
    if kind == "desktop_seq":
        conv = f"wbdesk-{now}"
        seq = desktop_chat_sequence(auth, conv, conv, conv)
        fp = desktop_fingerprint(auth)
        events = []
        for e in seq:
            m = dict(e)
            m.update(fp)
            m["activityId"] = ACTIVITY_ID
            events.append(m)
        # 桌面上报走 copilot 域；发 codebuddy.cn 域不点亮桌面任务
        return events, COPILOT, {"X-Product": "SaaS", "User-Agent": DESKTOP_UA}
    raise ValueError(f"未知 report_kind: {kind}")


# --------------------------------------------------------------------------
# 执行模式
# --------------------------------------------------------------------------
def run_account(auth, opts, stats):
    """--run：viewed 激活 -> 执行判据（share-complete/report 事件）-> 轮询 -> claim。"""
    uid8 = (auth.get("uid") or "")[:8] or "?"
    stats["accounts"] += 1
    try:
        tasks, in_period = fetch_school_tasks(auth["token"])
    except (AuthError, RuntimeError) as e:
        print(f"[school2026] {uid8} run 拉取任务失败: {e}")
        stats["fail"] += 1
        return
    if not in_period:
        print(f"[school2026] {uid8} 活动非进行期（in_period=false），跳过")
        return

    by_code = {t.get("task_code"): t for t in tasks}

    def refetch(code):
        try:
            ts, _ = fetch_school_tasks(auth["token"])
            return next((x for x in ts if x.get("task_code") == code), None)
        except (AuthError, RuntimeError):
            return None

    for t in tasks:
        code = t.get("task_code") or "?"
        status = (t.get("status") or "?").lower()
        spec = KNOWN_TASKS.get(code, {})
        mode = spec.get("mode", "unknown")

        if not t.get("task_code"):
            continue
        if status in ("completed", "claimed"):
            print(f"[school2026] {uid8} {code}: {status} 已完成/已领，跳过")
            stats["already"] += 1
            continue
        if mode == "manual":
            print(f"[school2026] {uid8} {code}: {status} -> 人工环节（{spec.get('note')}），跳过")
            stats["skip"] += 1
        elif mode in ("share", "report"):
            if not opts.yes:
                print(f"[school2026] {uid8} {code}: {status} -> {mode} 动作（dry-run 不写）")
                stats["pending"] += 1
                continue
            # 1) pending -> viewed 激活（接单，H5 行为）
            if status == "pending":
                try:
                    post_viewed(auth["token"], code)
                except (AuthError, RuntimeError) as e:
                    print(f"[school2026] {uid8} {code}: viewed 激活失败: {e}")
                    stats["fail"] += 1
                    continue
                print(f"[school2026] {uid8} {code}: viewed 激活 pending->in_progress")
                time.sleep(opts.gap)
            # 2) 触发完成判据。share 一次即完成；report 按 target-当前进度 循环触发
            #    （每次事件独立 +1）。
            cur_prog = t.get("progress") or 0
            if mode == "share":
                rounds = 1
            elif spec.get("report_kind") == "desktop_seq":
                rounds = 1  # 桌面 6 连一次 = 一次桌面对话；6 连内已有完整链
            else:
                rounds = max(1, (t.get("target_count") or 1) - cur_prog)
            progress_changed = False
            for ri in range(rounds):
                try:
                    if mode == "share":
                        st, r = post_share_complete(auth["token"])
                        print(f"[school2026] {uid8} {code}: share-complete {st} "
                              f"code={r.get('code') if isinstance(r, dict) else r}")
                    else:
                        events, host, extra_h = make_report_events(auth, spec.get("report_kind"))
                        st, r = report_events(auth, events, host=host, extra_headers=extra_h)
                        print(f"[school2026] {uid8} {code}: report #{ri+1}/{rounds} "
                              f"{len(events)} 事件 -> http={st} "
                              f"code={r.get('code') if isinstance(r, dict) else r}")
                except (AuthError, RuntimeError) as e:
                    print(f"[school2026] {uid8} {code}: {mode} 触发 #{ri+1} 失败: {e}")
                    break
                time.sleep(opts.gap)
                # 每轮回读一次，判断是否已到 target/completed，提早终止
                t2 = refetch(code)
                if not t2:
                    continue
                after_prog = t2.get("progress") or 0
                after = (t2.get("status") or "?").lower()
                if after_prog > cur_prog:
                    cur_prog = after_prog
                    progress_changed = True
                if after in ("completed", "claimed") or after_prog >= (t.get("target_count") or 1):
                    break
            # 3) 最终回读确认
            t2 = refetch(code)
            if not t2:
                print(f"[school2026] {uid8} {code}: 回读任务失败，跳过")
                stats["fail"] += 1
                continue
            after = (t2.get("status") or "?").lower()
            after_prog = t2.get("progress") or 0
            if after in ("completed", "claimed") or progress_changed:
                print(f"[school2026] {uid8} {code}: {status}/{t.get('progress')} -> "
                      f"{after}/{after_prog}（点亮）")
                stats["ok"] += 1
            else:
                print(f"[school2026] {uid8} {code}: {status} -> {after}（未变化）")
                stats["pending"] += 1
            # 4) 若 completed，claim 领奖（chance_granted）
            if after == "completed":
                try:
                    stc, rc = post_claim(auth["token"], code)
                    print(f"[school2026] {uid8} {code}: claim {stc} "
                          f"code={rc.get('code') if isinstance(rc, dict) else rc}")
                    time.sleep(opts.gap)
                except (AuthError, RuntimeError) as e:
                    print(f"[school2026] {uid8} {code}: claim 失败: {e}（可稍后手动补领）")
                    stats["fail"] += 1
        else:  # unknown
            print(f"[school2026] {uid8} {code}: {status} -> 未知任务类型（{t.get('title') or ''}），"
                  f"无分类证据，保守跳过")
            stats["skip"] += 1

    # 5) 任务循环收尾：抽奖段（claim 会发多次机会）。--yes 才真正 draw；
    #    dry-run 下 lottery_account 也只读查余额并打印将执行的调用，不发写请求。
    lottery_account(auth, opts, stats, count_account=False)


def lottery_account(auth, opts, stats, count_account=True):
    """抽奖段：查余额 -> 循环抽到空 -> 汇总打印。

    GET  /portal/activity/school/config      -> data.chance.balance（只读，抽前查余额）
      POST /portal/activity/school/wheel/draw  {draw_uuid} -> data.prize_code/credit_amount/
                                                             chance_balance（抽奖 + 扣次数）
      余额 0 时再抽返回 HTTP 409 code=40900 "no chance"（安全边界，脚本借此优雅停）。
    dry-run 只打印将执行的调用不发送。结果如实记录（含积分/实物券，不编造）。
    count_account=False：供 --run 任务循环收尾复用（账号数已由 run_account 计入）。"""
    uid8 = (auth.get("uid") or "")[:8] or "?"
    if count_account:
        stats["accounts"] += 1

    # dry-run：只读查余额可做；写（draw）只打印不发送
    if not opts.yes:
        try:
            _, chance = fetch_lottery_config(auth["token"])
        except (AuthError, RuntimeError) as e:
            print(f"[school2026] {uid8} lottery config 失败: {e}")
            stats["fail"] += 1
            return
        bal = chance.get("balance") or 0
        if bal <= 0:
            print(f"[school2026] {uid8} lottery balance=0，无需抽奖")
            return
        print(f"[school2026] {uid8} lottery balance={bal}（dry-run：将 POST /wheel/draw 抽 "
              f"{bal} 次，间隔 ≥1s，不发请求）")
        stats["pending"] += 1
        return

    try:
        cfg, chance = fetch_lottery_config(auth["token"])
    except (AuthError, RuntimeError) as e:
        print(f"[school2026] {uid8} lottery config 失败: {e}")
        stats["fail"] += 1
        return

    if not cfg.get("in_period"):
        print(f"[school2026] {uid8} 活动非进行期（in_period=false），抽奖跳过")
        return

    bal = chance.get("balance") or 0
    total_earned = chance.get("total_earned") or 0
    lottery_limit = chance.get("lottery_limit")
    if bal <= 0:
        print(f"[school2026] {uid8} lottery balance=0（累计获得 {total_earned} 次），无需抽奖")
        return

    print(f"[school2026] {uid8} lottery balance={bal} total_earned={total_earned} "
          f"lottery_limit={lottery_limit}（开始抽奖，抽到空为止）")

    results = []
    total_credit = 0
    prev_bal = bal
    stall = 0
    while bal > 0:
        draw_uuid = str(uuid.uuid4())  # 每轮一次性，抽后即弃
        try:
            st, r = post_lottery_draw(auth["token"], draw_uuid)
        except (AuthError, RuntimeError) as e:
            print(f"[school2026] {uid8} draw 失败: {e}")
            stats["fail"] += 1
            break
        if st != 200 or (isinstance(r, dict) and r.get("code") not in OK_CODES):
            code = (r or {}).get("code") if isinstance(r, dict) else None
            msg = (r or {}).get("message") if isinstance(r, dict) else r
            if code == 40900:
                print(f"[school2026] {uid8} draw http={st} code=40900（次数耗尽：no chance），"
                      f"按边界正常结束")
                bal = 0  # 「no chance」边界，如实结束
                break
            print(f"[school2026] {uid8} draw http={st} code={code} msg={msg}（未中奖，停）")
            break
        d = (r or {}).get("data") or {}
        prize_code = d.get("prize_code") or "?"
        credit = d.get("credit_amount") or 0
        bal = d.get("chance_balance", bal)  # 服务端回读余额，驱动循环
        if bal >= prev_bal:   # 余额未降：服务端异常，防死循环
            stall += 1
            if stall >= 3:
                print(f"[school2026] {uid8} draw 余额未递减（服务端异常），提前停")
                break
        else:
            stall = 0
        prev_bal = bal
        label = lottery_prize_text(prize_code, credit)
        results.append({"prize_code": prize_code, "credit": credit, "label": label})
        total_credit += credit
        print(f"[school2026] {uid8} draw -> {label}（balance {bal}）")
        if bal > 0:
            time.sleep(opts.gap)

    # 汇总（如实列出每个中奖物）
    if results:
        credit_detail = "+".join(str(x["credit"]) for x in results)
        print(f"[school2026] {uid8} lottery 汇总: {len(results)} 抽，积分增量 {total_credit}"
              f"（{credit_detail}）")
        for x in results:
            print(f"    - {x['label']}")
        stats["ok"] += 1 if bal == 0 else 0   # 只有余额归零才算抽完
        if bal > 0:
            stats["pending"] += 1              # 中途被服务端打断，未抽完
    else:
        print(f"[school2026] {uid8} lottery 无一抽出奖（上次余额 {bal}）")


def print_notes():
    """打印任务/未打通说明（对应头部风险提示，不编造）。"""
    print("")
    print("[school2026] 说明：非人工任务（share/chat/desktop/expert）自动点亮并领奖；"
          "task_student_verify 学生认证为人工环节，--run 跳过；"
          "幸运大转盘将在任务收尾或 --lottery-only 时抽到余额为 0。")


def main():
    ap = argparse.ArgumentParser(description="小程序开学季任务中心：只读盘点 + 事件上报自动化")
    ap.add_argument("accounts", nargs="*", help="uid 前缀（可多个）或 ALL；--token 时可不传")
    ap.add_argument("--list", action="store_true", help="只读盘点（默认行为）")
    ap.add_argument("--run", action="store_true", help="执行上报/领取动作（需 --yes 放行真实写）")
    ap.add_argument("--lottery-only", action="store_true",
                    help="只抽奖不做任务（读余额+抽到空）")
    ap.add_argument("--yes", action="store_true", help="放行写操作（配合 --run/--lottery-only）")
    ap.add_argument("--token", default=None, help="显式覆盖 token（不同源凭证逃生门）")
    ap.add_argument("--uid", default="", help="配合 --token 指定 uid（仅日志用）")
    ap.add_argument("--gap", type=float, default=1.5, help="写动作间隔秒数（默认 1.5，最小 1.0）")
    a = ap.parse_args()

    if a.gap < 1.0:
        a.gap = 1.0  # 写操作间隔 ≥1s

    stats = {"accounts": 0, "ok": 0, "already": 0, "skip": 0,
             "pending": 0, "fail": 0}

    run_mode = bool(a.run or a.lottery_only)   # 抽奖也是执行动作（写 draw）
    mode = "LOTTERY" if a.lottery_only else ("RUN" if a.run else "LIST")
    if run_mode and not a.yes:
        print(f"mode={mode} dry-run（写操作需 --yes 放行；{mode} 不带 --yes 只打印将发送的动作）")
    else:
        print(f"mode={mode} {'REAL' if a.yes else ''} gap={a.gap}")

    auths = []
    if a.token:
        auths.append({"token": a.token, "uid": a.uid or "", "nick": "--token--", "file": "<cli>"})
    else:
        prefixes = []
        if not a.accounts or (len(a.accounts) == 1 and a.accounts[0].upper() == "ALL"):
            # tc.iter_auth_files 已排除 zcode 等非 WorkBuddy 来源（扁平形落盘、
            # 无 CN 任务中心），避免 load_auth 抛错中断整批。
            prefixes = [os.path.basename(p)[10:18] for p in tc.iter_auth_files()]
        else:
            prefixes = a.accounts
        seen, prefixes2 = set(), []
        for p in prefixes:
            if p not in seen:
                seen.add(p)
                prefixes2.append(p)
        for p in prefixes2:
            try:
                auths.append(tc.load_auth(p))
            except SystemExit as e:
                print(f"ERR: {e}")

    if not auths:
        print("ERR: 无可用账号（检查 auths/ 或 --token）")
        sys.exit(1)

    for auth in auths:
        # global realm 不适用 CN 开学季任务/转盘：明确跳过、不发起任何请求。
        if tc.auth_is_global(auth):
            uid8 = (auth.get("uid") or "")[:8] or "?"
            print(f"[skip] {uid8} global realm 不适用 CN 任务")
            stats["skip"] += 1
            continue
        try:
            if a.lottery_only:
                lottery_account(auth, a, stats)
            elif run_mode:
                run_account(auth, a, stats)
            else:
                list_account(auth, stats)
        except (AuthError, RuntimeError) as e:
            uid8 = (auth.get("uid") or "")[:8] or "?"
            print(f"ERR: [school2026] {uid8} 处理失败: {e}")
            stats["fail"] += 1

    print(f"school2026 done: accounts={stats['accounts']} ok={stats['ok']} "
          f"already={stats['already']} skipped={stats['skip']} pending={stats['pending']} "
          f"fail={stats['fail']}")
    print_notes()
    sys.exit(2 if stats["fail"] > 0 else 0)


if __name__ == "__main__":
    main()