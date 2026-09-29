#!/usr/bin/env python3
"""task_runner —— 批量「查询 → 完成(点亮) → 领取」成长任务的一体化辅助工具.

用途
  对任意账号（uid 前缀 / ALL）查询成长任务现状，并对可伪造成就态的任务补齐上报
  点亮至 completed，最后经真实领奖端点入账。默认 dry-run（只查询展示），--yes 才写。

主要用法
  python3 task_runner.py <uid>                       # dry-run：查询展示任务现状
  python3 task_runner.py <uid> --yes --only-claim    # 只领奖（已完成任务直接入账，幂等）
  python3 task_runner.py <uid> --yes --only template_5   # 只完成/领取指定任务
  python3 task_runner.py <uid> --yes                 # 全量：accept→点亮→回读→claim
  python3 task_runner.py ALL --yes                      # 批量（写操作慎用，符合预算才执行）

参数
  account    uid 前缀或 ALL（ALL = auths/workbuddy-*.json 全部）
  --yes      真实执行写操作；缺省 dry-run 不发任何上报/accept/claim
  --only     只处理指定 task_code（可多次），同时过滤 dry-run 展示
  --only-claim 只领奖不点亮（对已完成但未领的账号直接入账；已领任务服务端幂等）
  --gap      上报/领奖动作间隔秒数，默认 1.0（所有写动作 ≥1s）

映射表摘要（task_code → 点亮事件 → 次数 → 对象 id 来源）
  常规上报经 POST {billing}/v2/report（必带 userId）：
    create_canvas        wbx_design_canvas_task_create      1  （自造 id wb-<ms>）
    template_5           agent_task_created_with_template   5  （scene ids: /console/as/support/scenes）
    expert_5             expert_actual_use                  5  (ex_ id: market expert/list)
    Expert_team_use_3    expert_actual_use(expertType=team) 3  （团队 id: market/COS expertType=team）
    skill_1              skill_info                         1  （skillId: market/skill/list）
    automation_1         automated_task_create_suc          1  （rrule 虚拟对象）
    playbook_prompt      playbook_prompt_send               1  （案例 id: static registry.json）
    Expert_lighthouse    expert_actual_use                  1  （轻量云 ex_ id: 关键词过滤）
    Hp_Appearance        appearance_skin_apply              1  （theme resourceKey: appearance/resources）
    chat_5               chat_request_send                  5  （5 条独立 conversation）
    Model_chat_GLM5.2    chat_request_send(glm-5.2)         1
    black_cat            chat_request_send(glm-5.2)         3  （★夜猫窗口 23-08 CST，窗口内最多补 1 次）
  领养链路（fork adoptBuddy：report 前置解锁 → agreement → buddy/first → claim）：
    first_buddy         领养第一只 Buddy                       1  （+300c+8e，buddy/first 直给积分）
  桌面指纹事件链（fork autotask.go 实测吸收：POST {chat}/v2/report + desktopFingerprint 注入）：
    RichMeow_Chat       桌面对话链 6 连事件组                   1  （agent_task_created→…→chat_request_response）
    Buddy_App/_QQ       buddyapp 五连（discover→…→bind_skip）  1  （application_id: open-platform search）
  web 域行为（fork ReportWebEvent 实测：POST www.workbuddy.cn/v2/report + web 指纹）：
    Library_read        web_element_click(library_doc_intro_click) 1  （space 文档 URL）
  小程序开学季任务（/portal/activity/school/*，模块 school_open_day_2026.py；accept=viewed 激活）：
    chat_3_times        小程序对话 3 次（mini chat_request_send+activityId 点亮） 3/每日
    expert_use          开学季专家对话（BackToSchool 专家 expert_actual_use）    1/每日
    share_invite        分享活动（share-complete 端点）                          1/每日
    task_student_verify 人工环节（跳过）
  小程序成长任务（growth 域 X-Client-Platform: miniprogram 专属下发，chat 域同头）：
    Sequential_Tasks_1  在小程序内完成 1 次有效对话（mini chat_request_send，
                        无 activityId——服务端按 source=mini_program 指纹关联） 100c+5e
    school_season      参与「校园日」有奖活动（jump 落 coffee-activity H5 = school 开学季
                        同一活动；mini chat_request_send + activityId 点亮）    100c+5e
  仍不可伪造：
    Expert_Philanthropy   真实捐款动作(M8)

领奖端点（M15 突破，勿用旧端点）
  POST {chat}/activity/growth/tasks/{task_code}/claim   —— 路径含 task_code、无 body（M15 实测）。
  旧 /v2/activity/growth/tasks/reward/claim 是 404 错端点。
  chat 域 400 时自动降级 web 域：POST https://www.workbuddy.cn/activity/growth/tasks/{code}/claim，
  带 Origin/Referer/x-client-platform: web 头（fork ClaimReward 同款，web_claim_fallback）。
  accept: POST {chat}/activity/growth/tasks/accept  body {"task_codes":[...]}（复数数组）。

来源：scripts/task_*.py 实测沉淀 + 各里程碑报告（M1-M15）+ 参考实现。
linguo2625469/workbuddy2api-panel（autotask.go/desktop.go/report.go/tasks.go 三账号实测）。
<uid> 已 14/14 claimed（credit +1450、energy +65），本脚本把相同桩点
知识固化为可对任意账号批量执行的 runner。

注意
  - 不修改 task_common.py（保持纯 HTTP 工具层）；本次映射逻辑全部留在本文件。
  - claim 用真实端点验证过幂等：已领任务返回 already_claimed，不重复入账。
  - first_buddy 走领养链路：report（前置解锁）→ agreement → buddy/first；buddy/first
    直接发 +300c+8e，任务变 completed 后再 claim（已是 already_claimed，幂等）。
  - richmeow/buddy 走 chat 域 /v2/report + 桌面指纹（ideName=WorkBuddy/extName=workbuddy-desktop，
    machineId/sessionId 由 uid+盐 md5 稳定派生，勿每次随机）；library 走 web 域 web 指纹。
  - 日志前缀 [task_runner]，行为 action ∈ query/accept/report/claim，汇总行 task_runner done: ...
"""
import sys, os, json, time, argparse, glob, urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc
# 小程序开学季任务模块（直接 import 复用其 MP 指纹/事件构造/领奖端点，勿复制实现）。
# 不抽到 task_common：task_common 是纯 growth/report HTTP 工具层（本文件头部注释约定），
# school_open_day_2026 才是小程序域指纹（MP_UA/extName=workbuddy-mp）与 school 端点的
# 唯一事实源；import 消耗为零（模块级只有常量定义，main() 有 __main__ 门）。
import school_open_day_2026 as school

# --------------------------------------------------------------------------
# 任务映射表（task_code -> 完成定义）
#   kind     事件构建器类型（build_event/ids_for 分派）
#   target   兜底次数（优先读服务端 progress.target）
#   src      对象 id 来源说明（仅展示）
#   unforgeable / reason   不可伪造任务的标注
# --------------------------------------------------------------------------
MAPPING = {
    "create_canvas":          {"kind": "canvas",     "target": 1, "src": "自造 wb-<ms>"},
    "template_5":             {"kind": "template",   "target": 5, "src": "scenes 场景 id"},
    "expert_5":               {"kind": "expert",     "target": 5, "src": "专家市场 ex_ id"},
    "Expert_team_use_3":      {"kind": "team",       "target": 3, "src": "团队 id"},
    "skill_1":                {"kind": "skill",      "target": 1, "src": "skillId"},
    "automation_1":           {"kind": "automation", "target": 1, "src": "rrule 虚拟对象"},
    "playbook_prompt":        {"kind": "playbook",   "target": 1, "src": "playbook 案例 id"},
    "Expert_lighthouse":      {"kind": "lighthouse", "target": 1, "src": "轻量云专家 id"},
    "Buddy_App":              {"kind": "buddy5",     "target": 1, "src": "buddy 应用 id"},
    "Buddy_App_QQ":           {"kind": "buddy5",     "target": 1, "src": "企鹅教师助手 id"},
    "Hp_Appearance":          {"kind": "skin",       "target": 1, "src": "主题 resourceKey"},
    "chat_5":                 {"kind": "chat",       "target": 5, "src": "无(独立会话)"},
    "Model_chat_GLM5.2":      {"kind": "glmchat",    "target": 1, "src": "无(glm-5.2)"},
    # 特殊：时段敏感（夜猫窗口 23-08 CST），窗口内最多补 1 次
    "black_cat":              {"kind": "cat",        "target": 3, "src": "无(glm-5.2 夜猫)"},
    # 领养链路（fork adoptBuddy：report 前置解锁 → agreement → buddy/first → claim）
    "first_buddy":            {"kind": "buddyfirst", "target": 1, "src": "无(report→agreement→first)"},
    # 桌面指纹 6 连对话事件链（fork DesktopChatSequence，三账号实测点亮）
    "RichMeow_Chat":          {"kind": "richmeow",   "target": 1, "src": "无(桌面指纹对话链)"},
    # web 域 web_element_click（fork ReportWebEvent，三账号实测点亮）
    "Library_read":           {"kind": "library",    "target": 1, "src": "无(资料库介绍点击)"},
    # 小程序开学季任务（school_open_day_2026.py 同款判据，accept=viewed 激活）
    "chat_3_times":           {"kind": "school", "target": 3, "src": "无(mini_chat×3+activityId)"},
    "expert_use":             {"kind": "school", "target": 1, "src": "BackToSchool 专家 id"},
    "share_invite":           {"kind": "school", "target": 1, "src": "无(share-complete)"},
    "desktop_chat_1_time":    {"kind": "school", "target": 1, "src": "无(桌面 6 连+activityId)"},
    # 小程序成长任务（growth 域小程序限定）：列表/accept/claim 均需 X-Client-Platform: miniprogram
    "Sequential_Tasks_1":     {"kind": "minichat", "target": 1, "src": "无(mini 对话无activityId)"},
    # 同下发口径的「校园日」任务：判据是 school 域 activityId 关联（mini chat + activityId）
    "school_season":          {"kind": "schoolseason", "target": 1, "src": "无(mini chat+activityId)"},
    # 不可伪造（真实业务副作用）
    "Expert_Philanthropy":    {"unforgeable": True, "reason": "真实捐款动作(M8)"},
}

# 夜猫时段（CST）23:00 - 次日 08:00
def within_night_window():
    try:
        import datetime as _dt
        now = _dt.datetime.now(_dt.timezone(_dt.timedelta(hours=8)))
        return now.hour >= 23 or now.hour < 8
    except Exception:
        return None


# --------------------------------------------------------------------------
# 稳定指纹派生（fork deriveID 同款思路：md5(uid+盐) 截短，注入用，勿每次随机）
# --------------------------------------------------------------------------
def derive_id(auth, salt):
    """由 uid 稳定派生一个 36 位 hex 设备标识（machineId/sessionId 复用）。

    幂等：同一账号每次生成相同值，模拟固定设备（fork desktop.go deriveID）。
    只用于事件指纹注入，不参与任何业务逻辑。
    """
    import hashlib
    return hashlib.md5(f"{salt}:{auth['uid']}".encode()).hexdigest()[:36]


def desktop_fingerprint(auth):
    """fork desktopFingerprint 同款公共桌面指纹（注入每个事件，覆盖同名业务键）。"""
    now = int(time.time() * 1000)
    return {
        "timezone": "Asia/Shanghai", "reportDelay": 2000,
        "userId": auth["uid"],
        "username": auth.get("nick", ""), "userNickname": auth.get("nick", ""),
        "product": "SaaS", "releaseDate": 1789036585355,
        "commit": "5f9692923c93033111c51ad7b003eb80204a9b75",
        "ideName": "WorkBuddy", "ideType": "WorkBuddy", "ideVersion": "5.5.6",
        "machineId": derive_id(auth, "machine"), "sessionId": derive_id(auth, "session"),
        "extName": "workbuddy-desktop", "extVersion": "5.5.6",
        "os": "win32", "arch": "x64", "osVersion": "10.0.26220",
        "cpuCores": 20, "memorySize": 24,
        "timestamp": now, "presentAt": now,
    }


def _desktop_headers(auth):
    """chat 域桌面指纹请求头（fork ReportDesktopEvent 同款）。"""
    return {
        "Accept": "application/json, text/plain, */*",
        "Content-Type": "application/json;charset=UTF-8",
        "User-Agent": DESKTOP_UA,
        "X-Domain": tc.chat_base(auth),
        "X-Product": "SaaS",
        "X-Request-ID": derive_id(auth, "req") + str(time.time_ns() % 1000000),
        "X-User-Id": auth["uid"],
    }


def _web_event_headers(auth, page_url):
    """web 域浏览器请求头（fork ReportWebEvent 同款，Library_read 用）。

    X-Domain 显式覆盖为 web 域：task_common._headers 会把 auth 的 domain
    （如 copilot.tencent.com）带上，发往 www.workbuddy.cn 会造成跨域不一致。
    """
    return {
        "Accept": "application/json",
        "Content-Type": "application/json",
        "x-client-platform": "web",
        "Origin": WEB_BASE,
        "Referer": page_url,
        "User-Agent": WEB_UA,
        "X-User-Id": auth["uid"],
        "X-Domain": WEB_BASE,
    }


def _web_claim_headers(auth):
    """web 域领奖头（fork ClaimReward 同款，web_claim_fallback 用）。"""
    return {
        "Authorization": "Bearer " + auth["token"],
        "Accept": "application/json, text/plain, */*",
        "Content-Type": "application/json",
        "Origin": WEB_BASE,
        "Referer": f"{WEB_BASE}/profile/growth-center",
        "x-client-platform": "web",
        "User-Agent": WEB_UA,
        "X-User-Id": auth["uid"],
        "X-Domain": WEB_BASE,
    }


# --------------------------------------------------------------------------
# 对象 id 来源（只读 fetch；均返回 [(id, meta_dict), ...] 有序去重列表）
# --------------------------------------------------------------------------
DEFAULT_SCENES = [(0, "幻灯片"), (2, "视频生成"), (4, "深度研究"), (6, "文档处理"),
                  (8, "数据分析"), (10, "可视化"), (12, "金融服务"), (14, "产品管理")]
DEFAULT_EXPERT_IDS = [
    "ContentCreator", "UiDesigner", "DataAnalyticsReporter", "ChinaEcommerceOperationsExpert",
    "DouyinStrategist", "SalesCoach", "BrandGuardian", "XiaohongshuOperationsExpert",
]
EXPERT_NAMES = {
    "ContentCreator": "内容创作专家", "UiDesigner": "UI设计师",
    "DataAnalyticsReporter": "数据分析报告师", "ChinaEcommerceOperationsExpert": "中国电商运营专家",
    "DouyinStrategist": "抖音策略师", "SalesCoach": "销售教练",
    "BrandGuardian": "品牌策略师", "XiaohongshuOperationsExpert": "小红书运营专家",
}
DEFAULT_TEAM_IDS = ["CloudOpsTeam", "SoftwareCompany", "TradingAgentTeam",
                    "GPTResearcherTeam", "MarketingCampaignTeam"]
DEFAULT_SKILL_IDS = [
    ("skill_2096525080079265792", "pptx"),
    ("skill_2096528888507297792", "xlsx"),
    ("skill_2070033533400236032", "qqmusic"),
    ("skill_2095322904487550976", "pdf"),
]
DEFAULT_CASES = [("worker-ledger-freedom-dashboard", {"title": "打工人小账本",
                  "artifact_type": "other", "categories": [""], "skills": [],
                  "experts": [], "prompt": ""})]
LIGHTHOUSE_IDS = [("ex_2cvvUZQhDyeJ", {
    "name": "腾讯轻量云专家", "category": "02-Engineering",
    "expertType": "agent", "version": "1.0.2"})]
LIGHTHOUSE_KEYWORDS = ["lighthouse", "轻量云"]

# 连登/能量读数（只读 oracle，M15 实测 /v2/activity/growth/energy -> data.balance）
PATH_ENERGY = "/v2/activity/growth/energy"
# buddyapp 五连事件承载应用（fork DesktopBuddyAppSequence 实测用企鹅教师助手，
# 同一组事件同时满足 Buddy_App 与 Buddy_App_QQ；两账号实测点亮）
BUDDY_APPS = {"Buddy_App": ("cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手"),
              "Buddy_App_QQ": ("cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手")}
PE_THEME = ("theme-tkmw7j", {"name": "和平精英激战金秋", "vipLevel": "free",
                             "series": "craft", "appearance": "light"})

COS_EXPERT_URL = ("https://acc-1258344699.cos.accelerate.myqcloud.com/"
                  "workbuddy/expert-marketplace/expert_center.json")
PLAYBOOK_BASE = "https://static.workbuddy.cn/workbuddy/playbook"
MARKET_LIST_PATH = "/v2/operation-platform/market/expert/list"
WEB_BASE = "https://www.workbuddy.cn"

# fork 桌面指纹（desktop.go）/ web 域（report.go）吸收常量
DESKTOP_UA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
WEB_UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
          "(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36")
# Library_read 空间文档 URL（fork autotask.go:438 实测三账号点亮）
LIBRARY_DOC_URL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"


def _fetch_scenes(auth):
    """GET /console/as/support/scenes -> [(scene_id, name)]（只读）。"""
    out = []
    try:
        st, d = tc.do_get(auth, tc.chat_base(auth), "/console/as/support/scenes?locale=zh-CN")
        if st == 200 and (d.get("code") in (0, None)):
            for s in (d.get("data") or {}).get("scenes") or []:
                if s.get("id") is not None:
                    out.append((str(s["id"]), s.get("name") or ""))
    except Exception as ex:
        print(f"  [warn] 场景清单拉取失败({ex})，回落内置表")
    if not out:
        out = [(str(i), n) for i, n in DEFAULT_SCENES]
    return out


def _fetch_market_experts(auth, team_only=False, keyword=None):
    """运行时专家市场 POST expert/list -> [(ex_id, meta)]。team_only 只留 expert_type=team。"""
    out, seen = [], set()
    try:
        for page in range(1, 4):
            body = {"page": page, "page_size": 50}
            if keyword:
                body["keyword"] = keyword
            st, r = tc.do_post(auth, tc.chat_base(auth), MARKET_LIST_PATH, body)
            if st != 200:
                break
            experts = ((r.get("data") or {}).get("experts") or [])
            if not experts:
                break
            for e in experts:
                eid = e.get("expert_id") or e.get("source_id")
                if not eid or eid in seen:
                    continue
                etype = e.get("expert_type") or "agent"
                if team_only and etype != "team":
                    continue
                seen.add(eid)
                out.append((eid, {
                    "name": e.get("display_name_zh") or e.get("profession_zh") or "",
                    "category": (e.get("categories") or [""])[0] or "",
                    "expertType": etype, "version": e.get("version") or "",
                    "agent_name": e.get("agent_name") or "",
                }))
            if len(experts) < 50:
                break
    except Exception as ex:
        print(f"  [warn] 专家市场拉取失败({ex})，回落内置表")
    # COS 静态清单补足（expert_center.json）：团队专家市场列表很薄（250 个里仅 2 个 team），
    # M4 实测 COS 有 52 个 team，作为 team_only 的主/补来源；普通专家用市场 ex_ id。
    try:
        req = urllib.request.Request(COS_EXPERT_URL, headers={"User-Agent": "Mozilla/5.0"})
        with urllib.request.urlopen(req, timeout=20) as rr:
            data = json.loads(rr.read().decode("utf-8", "replace"))
        seen = {i for i, _ in out}
        for e in (data.get("experts") or []):
            eid = e.get("id")
            etype = e.get("expertType") or "agent"
            if not eid or (team_only and etype != "team"):
                continue
            if eid in seen:
                continue
            seen.add(eid)
            prof = e.get("profession") or {}
            out.append((eid, {"name": prof.get("zh") or "",
                              "category": e.get("categoryId") or "",
                              "expertType": etype, "version": ""}))
    except Exception as ex2:
        print(f"  [warn] COS 专家清单失败({ex2})")
    if not out:
        ids = DEFAULT_TEAM_IDS if team_only else DEFAULT_EXPERT_IDS
        out = [(i, {"name": EXPERT_NAMES.get(i, ""), "category": "",
                    "expertType": "team" if team_only else "agent", "version": ""}) for i in ids]
    return out


def _fetch_skills(auth):
    """POST market/skill/list -> [(skill_id, name)]（只读）。"""
    out = []
    try:
        st, r = tc.do_post(auth, tc.chat_base(auth),
                           "/v2/operation-platform/market/skill/list",
                           {"page": 1, "page_size": 8})
        if st == 200:
            for s in (r.get("data") or {}).get("skills") or []:
                if s.get("skill_id"):
                    out.append((s["skill_id"], s.get("name") or ""))
    except Exception as ex:
        print(f"  [warn] 技能市场拉取失败({ex})，回落内置表")
    if not out:
        out = DEFAULT_SKILL_IDS
    return out


def _fetch_playbook_cases():
    """只读拉灵感案例注册表 -> [(case_id, meta)]。失败回落内置表。"""
    out = []
    try:
        req = urllib.request.Request(f"{PLAYBOOK_BASE}/registry.json",
                                     headers={"User-Agent": "Mozilla/5.0"})
        with urllib.request.urlopen(req, timeout=20) as r:
            data = json.loads(r.read().decode("utf-8", "replace"))
        for c in (data.get("cases") or []):
            if c.get("id"):
                out.append((c["id"], {"title": c.get("title") or "",
                                      "artifact_type": c.get("artifact_type") or "other",
                                      "categories": c.get("categories") or [],
                                      "skills": c.get("skills") or [],
                                      "experts": c.get("experts") or [],
                                      "prompt": c.get("prompt") or "",
                                      "source": "discover"}))
    except Exception as ex:
        print(f"  [warn] 案例注册表拉取失败({ex})，回落内置表")
    if not out:
        out = DEFAULT_CASES
    return out


def _fetch_lighthouse(auth):
    """市场关键词过滤轻量云专家 -> [(ex_id, meta)]。"""
    out = []
    for kw in LIGHTHOUSE_KEYWORDS:
        for eid, meta in _fetch_market_experts(auth, keyword=kw):
            blob = (eid + meta.get("name", "") + meta.get("agent_name", "")).lower()
            if any(k in blob for k in ["lighthouse", "轻量", "light", "yun"]):
                out.append((eid, meta))
    if not out:
        out = LIGHTHOUSE_IDS
    return out


def _fetch_pe_theme(auth):
    """appearance/resources 目录过滤和平精英 -> [(resource_key, meta)]。"""
    out = []
    try:
        st, r = tc.do_post(auth, tc.billing_base(auth),
                           "/v2/operation-platform/appearance/resources",
                           {"platform": "client", "kind": "theme",
                            "version": tc.CLIENT_UA.split("/")[-1], "lang": "zh-CN"})
        if st == 200:
            for x in (r.get("data") or {}).get("resources") or []:
                nm = x.get("name") or ""
                if "和平精英" in nm or "pubg" in nm.lower():
                    out.append((x.get("id") or "", {"name": nm,
                                "vipLevel": x.get("vip_level", ""),
                                "series": x.get("series", ""),
                                "appearance": x.get("appearance", "")}))
    except Exception as ex:
        print(f"  [warn] 主题目录拉取失败({ex})，回落内置表")
    if not out:
        out = [PE_THEME]
    return out


def _dedup_slice(src, offset, need):
    """src: [(id, meta)]；去重后按 offset 偏移取 need 个。"""
    seen, final = set(), []
    for i, m in src:
        if i not in seen:
            seen.add(i)
            final.append((i, m))
    return final[offset:offset + need]


# 小程序开学季任务 → school 模块的 report_kind/处理方式映射（school_open_day_2026.KNOWN_TASKS）
# mode=manual（task_student_verify 学生认证）不在 MAPPING，未映射任务照旧保守跳过。
SCHOOL_REPORT_KIND = {
    "chat_3_times": "mini_chat",
    "expert_use": "expert",
    "share_invite": "share",        # 非事件上报，走 /tasks/share-complete 端点
    "desktop_chat_1_time": "desktop_seq",
}

# growth 域小程序限定任务：列表/accept/claim 端点对 X-Client-Platform 敏感（缺头时任务
# 不下发、accept 返回 task not found）。web 小程序 H5 在请求拦截器统一注入该头
# （config-D_y8qqW1.js: E() 判 session platform=miniProgram，te()=$:miniprogram）。
MP_PLATFORM_HEADER = {"X-Client-Platform": "miniprogram"}


def ids_for(kind, auth, need, offset=0):
    """返回需要上报的对象 id 列表 [(obj_id, meta)]（按源顺序，偏移 cur 避免复用）。"""
    if need <= 0:
        return []
    if kind == "canvas":
        base = int(time.time() * 1000)
        return [(f"wbx-canvas-{base + i}", {}) for i in range(need)]
    if kind == "template":
        return _dedup_slice(_fetch_scenes(auth), offset, need)
    if kind == "expert":
        return _dedup_slice(_fetch_market_experts(auth), offset, need)
    if kind == "team":
        return _dedup_slice(_fetch_market_experts(auth, team_only=True), offset, need)
    if kind == "skill":
        return _dedup_slice(_fetch_skills(auth), offset, need)
    if kind == "playbook":
        return _dedup_slice(_fetch_playbook_cases(), offset, need)
    if kind == "lighthouse":
        return _dedup_slice(_fetch_lighthouse(auth), offset, need)
    if kind == "skin":
        return _dedup_slice(_fetch_pe_theme(auth), offset, need)
    if kind in ("chat", "glmchat", "automation", "cat"):
        return [("", {}) for _ in range(need)]
    if kind == "richmeow":
        return [("", {}) for _ in range(need)]
    if kind in ("buddy5", "library", "buddyfirst", "minichat", "schoolseason"):
        # history/current 不是顺序语义：buddy5/library/buddyfirst 是固定事件组按需补 1 次（offset 无意义）
        return [("", {}) for _ in range(need)]
    if kind == "school":
        # school 任务的进度语义同上（每次事件独立 +1，无对象 id）；school 域进度不在 growth 任务里
        return [("", {}) for _ in range(need)]
    return []


# --------------------------------------------------------------------------
# 桌面指纹事件链（fork desktop.go 吸收：agent_task_created→chat_message_response→…
# 6 连事件组 + 五连 buddyapp 事件组；ReportDesktopEvent 注入桌面指纹到每个事件）
# --------------------------------------------------------------------------
def desktop_chat_sequence(auth, conversation_id, request_id, message_id,
                          model_id="fast-model", model_name="fast-model"):
    """fork DesktopChatSequence 同款 6 连「桌面端成功对话」事件链（点亮 RichMeow_Chat）。"""
    now = int(time.time() * 1000)
    ev = []

    def mk(code, extra):
        e = {"eventCode": code}
        e.update(extra)
        ev.append(e)

    mk("agent_task_created", {
        "source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
        "requestModelId": model_id, "requestModelName": model_name,
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
        "codebuddy.session_id":              conversation_id,
        "codebuddy.conversation_request_id": request_id,
    })
    mk("chat_message_response", {
        "messageId": message_id + "-assistant", "responseModelId": model_id,
        "inputToken": 120, "outputToken": 80, "totalToken": 200,
        "cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
        "isSuccessful": True, "messageErrorCode": "", "finishReason": "stop",
        "firstTokenAt": now, "traceId": request_id,
        "conversationId": conversation_id,
        "rootRequestId": request_id, "parentConversationId": conversation_id,
        "agentName": "cli", "agentType": "main",
        "codebuddy.session_id":              conversation_id,
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


def desktop_buddy5_sequence(auth, buddy_id, buddy_name):
    """fork DesktopBuddyAppSequence 同款五连「进入 Buddy 应用」事件（点亮 Buddy_App/_QQ）。

    discover → show → enter_click → auth_confirm_click → bindaccount_skip_click，
    一次上报即点亮；服务端不校验真实授权（fork 两账号实测）。
    """
    ev = []

    def mk(code, extra):
        e = {"eventCode": code, "mode": "LOCAL",
             "buddyId": buddy_id, "buddyName": buddy_name}
        e.update(extra)
        ev.append(e)

    mk("buddyapp_discover_click", {})
    mk("buddyapp_show", {"elementId": buddy_id, "elementName": buddy_name, "position": 2})
    mk("buddyapp_enter_click",
       {"elementId": buddy_id, "elementName": buddy_name, "position": 2, "isFirstPage": "1"})
    mk("buddyapp_auth_confirm_click", {"elementId": buddy_id, "elementName": buddy_name})
    mk("buddyapp_bindaccount_skip_click", {"elementId": buddy_id, "elementName": buddy_name})
    return ev


# --------------------------------------------------------------------------
# 上报通道（fork ReportDesktopEvent / ReportWebEvent 同款）
# --------------------------------------------------------------------------
def report_desktop_events(auth, events):
    """以桌面指纹向 {chat}/v2/report 批量上报；返回业务信封 (st, code)。

    fork ReportDesktopEvent：每个事件（列表）注入 desktopFingerprint 公共字段覆盖同名键。
    """
    fp = desktop_fingerprint(auth)
    arr = []
    for e in events:
        m = dict(e)
        m.update(fp)
        arr.append(m)
    st, r = tc.do_post(auth, tc.chat_base(auth), tc.PATH_REPORT, arr,
                       headers=_desktop_headers(auth))
    sc = r.get("code") if isinstance(r, dict) else r
    return st, sc


def report_web_event(auth, event_code, page_url, element_id, element_name):
    """以 web 域指纹向 https://www.workbuddy.cn/v2/report 上报单事件（Library_read）。

    fork ReportWebEvent：浏览器形状（os/machineId/userAgent），带 x-client-platform: web。
    """
    now = int(time.time() * 1000)
    ev = {
        "eventCode": event_code, "timestamp": now, "reportDelay": 0,
        "pageURL": page_url, "elementId": element_id, "elementName": element_name,
        "os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": WEB_UA,
        "machineId": derive_id(auth, "webmachine"), "userId": auth["uid"],
        "userNickname": auth.get("nick", ""),
    }
    st, r = tc.do_post(auth, WEB_BASE, tc.PATH_REPORT, [ev],
                       headers=_web_event_headers(auth, page_url))
    sc = r.get("code") if isinstance(r, dict) else r
    return st, sc


# --------------------------------------------------------------------------
# 事件构建器（照抄各 task_*.py 已验证形状，必带 userId）
# --------------------------------------------------------------------------
def build_event(auth, kind, obj_id, meta, idx):
    now = int(time.time() * 1000)
    cid = f"wb-run-{now}-{idx}"
    rid = f"{cid}-{now}"
    uid = auth["uid"]
    # meta 可能是 dict（丰富元数据）或 str（如技能名）；统一成 dict 供 m.get 使用
    if isinstance(meta, str):
        m = {"name": meta}
    elif isinstance(meta, dict):
        m = meta
    else:
        m = {}

    if kind == "canvas":
        return {"eventCode": "wbx_design_canvas_task_create", "timestamp": now,
                "reportDelay": 0, "conversationId": cid, "requestId": rid,
                "source": "summon_keyword", "isCustomModel": False, "name": "",
                "inputLength": 12, "id": obj_id or f"wbx-canvas-{now}",
                "cost": 0, "isSuccessful": True, "userId": uid}

    if kind == "template":
        return {"eventCode": "agent_task_created_with_template", "timestamp": now,
                "reportDelay": 0, "isCustomModel": True, "id": str(obj_id),
                "name": m or "", "requestId": rid, "conversationId": cid, "userId": uid}

    if kind == "expert":
        return {"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
                "mode": "CLOUD", "id": obj_id, "name": m.get("name") or obj_id,
                "expertTitle": m.get("name") or "", "type": m.get("category") or "",
                "expertType": m.get("expertType") or "agent", "source": "builtin",
                "version": m.get("version") or "", "cost": 0, "characterCount": 12,
                "conversationId": cid, "requestId": rid, "messageId": rid,
                "requestModelId": "deepseek-v4-flash", "requestModelName": "DeepSeek V4 Flash",
                "userId": uid}

    if kind == "team":
        return {"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
                "mode": "CLOUD", "id": obj_id, "name": m.get("name") or obj_id,
                "expertTitle": m.get("name") or "", "type": m.get("category") or "",
                "expertType": "team", "source": "builtin", "version": m.get("version") or "",
                "cost": 0, "characterCount": 12, "conversationId": cid, "requestId": rid,
                "messageId": rid, "requestModelId": "deepseek-v4-flash",
                "requestModelName": "DeepSeek V4 Flash", "userId": uid}

    if kind == "skill":
        sname = meta if isinstance(meta, str) else (meta.get("name") if isinstance(meta, dict) else "")
        return {"eventCode": "skill_info", "timestamp": now, "reportDelay": 0,
                "skillId": obj_id, "skillName": sname or obj_id,
                "skillVersion": "", "action": "use", "conversationId": cid,
                "requestId": rid, "userId": uid}

    if kind == "automation":
        return {"eventCode": "automated_task_create_suc", "timestamp": now,
                "reportDelay": 0, "name": "每周五自动生成周报", "source": "manually",
                "modelId": "deepseek-v4-flash", "modelIsThinking": False,
                "expertId": "", "expertMarketplace": "", "connectorIds": "",
                "connectorCount": 0, "skills": "", "skillCount": 0,
                "scheduleType": "recurring", "pushToWeChat": False, "pushToWecomBot": False,
                "conversationId": cid, "requestId": rid,
                "schedule": {"type": "recurring", "rrule": "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0"},
                "prompt": "每周五自动整理本周工作，生成一份周报。", "userId": uid}

    if kind == "playbook":
        skills = m.get("skills") or []
        experts = m.get("experts") or []
        cat_id = (m.get("categories") or [""])[0] or ""
        return {"eventCode": "playbook_prompt_send", "timestamp": now, "reportDelay": 0,
                "id": obj_id, "name": m.get("title") or obj_id,
                "type": m.get("artifact_type") or "other",
                "promptLength": len(m.get("prompt") or ""), "isOfficial": 1,
                "skills": ",".join(s.get("skill_id", "") for s in skills if s.get("skill_id")),
                "skillNames": ",".join(s.get("name", "") for s in skills if s.get("name")),
                "expertId": (experts[0].get("expert_id") or experts[0].get("id")) if experts else "",
                "expertName": (experts[0].get("name") or "") if experts else "",
                "categoryId": cat_id, "categoryName": "", "query": "",
                "source": "discover", "conversationId": cid, "requestId": rid,
                "ext1": "discover", "userId": uid}

    if kind == "lighthouse":
        return {"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
                "mode": "CLOUD", "id": obj_id, "name": m.get("name") or obj_id,
                "expertTitle": m.get("name") or "", "type": m.get("category") or "",
                "expertType": m.get("expertType") or "agent", "source": "builtin",
                "version": m.get("version") or "", "cost": 0, "characterCount": 12,
                "conversationId": cid, "requestId": rid, "messageId": rid,
                "requestModelId": "deepseek-v4-flash", "requestModelName": "DeepSeek V4 Flash",
                "userId": uid}

    if kind == "skin":
        return {"eventCode": "appearance_skin_apply", "timestamp": now, "reportDelay": 0,
                "action": "apply", "source": "settings_close", "id": obj_id,
                "vipLevel": m.get("vipLevel") or "free", "series": m.get("series") or "craft",
                "type": "unknown", "name": m.get("name") or obj_id, "userId": uid}

    if kind in ("chat", "glmchat", "cat"):
        model_id = "glm-5.2" if kind in ("glmchat", "cat") else "deepseek-v4-flash"
        model_name = "GLM-5.2" if kind in ("glmchat", "cat") else "DeepSeek V4 Flash"
        mode = "night" if kind == "cat" else "craft"
        return {"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
                "mode": mode, "conversationId": cid, "requestId": cid,
                "inputLength": 12, "requestModelId": model_id, "requestModelName": model_name,
                "isPlan": False, "isAutoExecuteTerminal": False, "isAutoModify": False,
                "codebaseEnable": False, "maxToken": 0, "maxSteps": 0, "temperature": 0,
                "maxRetries": 0, "mentionContexts": [], "knowledgeId": [],
                "knowledgeName": [], "codebaseId": "", "mentionContextCount": 0,
                "command": "", "expertId": "", "recommendId": "", "skillId": "",
                "skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": now,
                "traceId": "", "rootRequestId": cid, "parentConversationId": cid,
                "agentName": "default", "agentType": "conversation", "userId": uid}

    if kind == "minichat":
        # 小程序成长任务（Sequential_Tasks_1）判据：mini 指纹 chat_request_send，
        # 无 activityId（growth 域按 source=mini_program 指纹关联，实测 0ceb9c7c 点亮）。
        # 形状对齐 school.mini_chat_event，仅去掉 activityId、extVersion 修正为 2.4.0。
        return {"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
                "source": "mini_program", "ideName": "wx_app_cloud",
                "ideType": "WorkBuddy_MP", "extName": "workbuddy-mp",
                "extVersion": "2.4.0", "mode": "chat",
                "conversationId": cid, "requestId": cid, "inputLength": 12,
                "mentionContexts": [], "mentionContextCount": 0, "userId": uid}

    if kind == "schoolseason":
        # growth 域 school_season（校园日）判据：mini 指纹 chat_request_send +
        # activityId=school_open_day_2026（与 school 域开学季同 activityId 关联，
        # 实测 0ceb9c7c 点亮；无 activityId 的事件不点亮）。直接复用 school 模块
        # 构造器保证字段与 school 段单一事实源（勿在此手抄第二份形状）。
        return school.mini_chat_event(auth, f"wb-run-{now}-{idx}")

    raise ValueError(f"unknown kind: {kind}")


# --------------------------------------------------------------------------
# 领奖（M15 真实端点：POST {chat}/activity/growth/tasks/{code}/claim，无 body）
# 400 自动降级 web 域：POST https://www.workbuddy.cn/activity/growth/tasks/{code}/claim，
# 带 Origin/Referer/x-client-platform: web（fork ClaimReward 同款，web_claim_fallback）。
# --------------------------------------------------------------------------
def _claim_parse(auth, code, uid8, stats, gap, st, r, via_web=False):
    """解析 claim 响应信封；业务判定成功/已领/失败。via_web 仅用于日志标注。"""
    tag = " web" if via_web else ""
    if st != 200 or not isinstance(r, dict) or r.get("code") != 0:
        msg = r.get("msg") if isinstance(r, dict) else r
        print(f"[task_runner] {uid8} {code}: claim{tag} {st} {msg} -> ERR")
        stats["fail"] += 1
        return 0
    data = r.get("data") or {}
    credit = data.get("credit") or 0
    energy = data.get("energy") or 0
    if data.get("already_claimed"):
        print(f"[task_runner] {uid8} {code}: claim{tag} 200 already_claimed(credit=+{credit} energy=+{energy})")
        stats["already"] += 1
        time.sleep(gap)
        return 2
    print(f"[task_runner] {uid8} {code}: claim{tag} 200 ok(credit=+{credit} energy=+{energy})")
    stats["ok"] += 1
    stats["credit"] += credit
    stats["energy"] += energy
    time.sleep(gap)
    return 1


def _claim_via_web(auth, code, uid8, stats, gap):
    """web 域领奖（fork ClaimReward 同款头：Origin/Referer/x-client-platform: web）。"""
    st, r = tc.do_post(auth, WEB_BASE,
                       f"/activity/growth/tasks/{code}/claim", None,
                       headers=_web_claim_headers(auth))
    return _claim_parse(auth, code, uid8, stats, gap, st, r, via_web=True)


def claim_one(auth, code, uid8, stats, gap, mp=False):
    """领单个任务。返回 1=新入账 2=already_claimed 0=失败。

    chat 域（M15 实测成功）先行；400 则自动降级 web 域带完整头（web_claim_fallback）。
    幂等语义保持：already_claimed 不算失败（返回 2）。
    mp=True 时带 X-Client-Platform: miniprogram（小程序限定任务 claim 必需）。
    """
    st, r = tc.do_post(auth, tc.chat_base(auth),
                       f"/activity/growth/tasks/{code}/claim", None,
                       headers=MP_PLATFORM_HEADER if mp else None)
    if st != 200 or not isinstance(r, dict) or r.get("code") != 0:
        msg = r.get("msg") if isinstance(r, dict) else r
        # 400 降级到 web 域（fork 实测 Web 成长中心端点）：chat 域路径对部分任务 400
        if st == 400:
            print(f"[task_runner] {uid8} {code}: claim 400 {msg} -> 降级 web 域")
            return _claim_via_web(auth, code, uid8, stats, gap)
        print(f"[task_runner] {uid8} {code}: claim {st} {msg} -> ERR")
        stats["fail"] += 1
        return 0
    return _claim_parse(auth, code, uid8, stats, gap, st, r)


# --------------------------------------------------------------------------
# 小程序开学季任务（school 域）：独立任务空间 /portal/activity/school/tasks，
# 复用 school_open_day_2026.py 的全部判据与端点（同指纹、同 accept→claim 链路）。
# 与 growth 任务不同点：accept 动作是 POST /tasks/{code}/viewed；领奖走
# POST /tasks/{code}/claim（school 域专属，非 growth claim 端点）。
# --------------------------------------------------------------------------
SCHOOL_CODES = [c for c in SCHOOL_REPORT_KIND]


def school_fetch_tasks(auth):
    """school 域任务列表（复用 school 模块）。返回 (tasks, in_period)；失败抛异常。"""
    return school.fetch_school_tasks(auth["token"])


def _school_find(tasks, code):
    return next((t for t in tasks if t.get("task_code") == code), None)


def process_school_task(auth, code, t, st_tasks, uid8, opts, stats):
    """单个 school 任务：viewed 激活 → 判据触发（按 target-进度 循环）→ 回读 → claim。

    判据触发全部复用 school.make_report_events（mini_chat/expert/desktop_seq）与
    school.post_share_complete；幂等：completed/claimed 是正常态直接跳过。
    """
    def refetch():
        try:
            ts, _ = school_fetch_tasks(auth)
            return _school_find(ts, code)
        except Exception:
            return None

    status = (t.get("status") or "?").lower()
    cur = t.get("progress") or 0
    target = t.get("target_count") or 1
    reward = t.get("reward_credit") or 0
    stats["total"] += 1

    if status in ("completed", "claimed"):
        print(f"[task_runner] {uid8} {code}: query {status}({cur}/{target}) -> 已完成/已领，跳过")
        stats["already"] += 1
        return

    report_kind = SCHOOL_REPORT_KIND.get(code)
    if report_kind is None:
        print(f"[task_runner] {uid8} {code}: query {status} -> school 未映射(人工/未知)，skip")
        stats["skip"] += 1
        return

    if not opts.yes:
        print(f"[task_runner] {uid8} {code}: query {status}({cur}/{target}) reward={reward} "
              f"-> school {report_kind} 判据（dry-run 跳过）")
        stats["pending"] += 1
        return

    # 1) pending -> viewed 激活（「接任务」，H5 行为；复用 school 模块端点）
    if status == "pending":
        try:
            st, r = school.post_viewed(auth["token"], code)
            print(f"[task_runner] {uid8} {code}: viewed 激活 {st} "
                  f"code={r.get('code') if isinstance(r, dict) else r}")
            time.sleep(opts.gap)
        except Exception as e:
            print(f"[task_runner] {uid8} {code}: viewed 失败: {e}")
            stats["fail"] += 1
            return

    # 2) 判据触发：share 一次即完成；desktop_seq 一次 6 连 = 一次对话；
    #    mini_chat/expert 按 target-当前进度 循环（每次事件独立 +1）。
    if report_kind == "share":
        rounds = 1
    elif report_kind == "desktop_seq":
        rounds = 1
    else:
        rounds = max(1, target - cur)

    progressed = False
    for ri in range(rounds):
        try:
            if report_kind == "share":
                st, r = school.post_share_complete(auth["token"])
            else:
                events, host, extra_h = school.make_report_events(auth, report_kind)
                st, r = school.report_events(auth, events, host=host, extra_headers=extra_h)
            print(f"[task_runner] {uid8} {code}: report {ri + 1}/{rounds} {st} "
                  f"code={r.get('code') if isinstance(r, dict) else r} ({report_kind})")
        except Exception as e:
            print(f"[task_runner] {uid8} {code}: report 失败 #{ri + 1}: {e}")
            stats["fail"] += 1
            break
        time.sleep(opts.gap)
        t2 = refetch()
        if t2 is None:
            continue
        after = (t2.get("status") or "?").lower()
        after_prog = t2.get("progress") or 0
        if after_prog > cur:
            cur, progressed = after_prog, True
        if after in ("completed", "claimed") or after_prog >= target:
            break

    # 3) 回读 + claim（school 域端点，发抽奖机会/积分）
    t2 = refetch() or t
    after = (t2.get("status") or "?").lower()
    after_prog = t2.get("progress") or 0
    if after == "claimed":
        print(f"[task_runner] {uid8} {code}: {status}/{cur} -> claimed（本轮已入账）")
        stats["already"] += 1
        return
    if after == "completed" or after_prog >= target:
        print(f"[task_runner] {uid8} {code}: {status} -> {after}/{after_prog}（点亮）")
        try:
            stc, rc = school.post_claim(auth["token"], code)
            print(f"[task_runner] {uid8} {code}: claim {stc} "
                  f"code={rc.get('code') if isinstance(rc, dict) else rc}")
            stats["ok"] += 1
            time.sleep(opts.gap)
        except Exception as e:
            print(f"[task_runner] {uid8} {code}: claim 失败: {e}（可稍后补领）")
            stats["fail"] += 1
        return
    if progressed:
        print(f"[task_runner] {uid8} {code}: {status} -> {after}/{after_prog}（部分点亮，未达 target）")
        stats["ok"] += 1
    else:
        print(f"[task_runner] {uid8} {code}: {status} -> {after}/{after_prog}（未变化）")
        stats["pending"] += 1


def process_school_tasks(auth, opts, stats, uid8):
    """账号级 school 段：拉 school 任务列表并逐个处理。异常/非活动期如实标注。"""
    try:
        tasks, in_period = school_fetch_tasks(auth)
    except Exception as e:
        print(f"[task_runner] {uid8} school/tasks 拉取失败: {e}")
        stats["fail"] += 1
        return
    if not in_period:
        print(f"[task_runner] {uid8} school 活动非进行期（in_period=false），school 段跳过")
        return
    print(f"[task_runner] {uid8} school/tasks in_period={in_period} tasks={len(tasks)}")
    for code in SCHOOL_CODES:
        t = _school_find(tasks, code)
        if t is None:
            continue
        if opts.only_codes and code not in opts.only_codes:
            continue
        process_school_task(auth, code, t, tasks, uid8, opts, stats)


# --------------------------------------------------------------------------
# 小程序成长任务（growth 域小程序限定，Sequential_Tasks_1 / school_season）
# --------------------------------------------------------------------------
def _mp_list_tasks(auth):
    """带 X-Client-Platform: miniprogram 拉任务列表（小程序限定任务仅在该口径下发）。"""
    st, r = tc.do_get(auth, tc.chat_base(auth), tc.PATH_LIST_TASKS,
                      headers=MP_PLATFORM_HEADER)
    if st != 200 or not isinstance(r, dict):
        raise RuntimeError(f"mp list_tasks http={st}")
    return (r.get("data") or {}).get("tasks") or []


def _mp_task_status(auth, code):
    """mp 口径查单任务；无此任务返回 None。"""
    return next((t for t in _mp_list_tasks(auth) if t.get("task_code") == code), None)


def process_minichat_task(auth, code, opts, stats, uid8):
    """小程序成长任务：mp 口径查询 → accept → mini 对话上报 → 回读 → claim。

    与 process_task 的 growth 主循环分开处理：该任务在默认（无 mp 头）列表里不存在，
    accept/claim 也要求同一头（缺头 accept 返回 task not found，实测）。
    适用于同一 mp 下发口径的 Sequential_Tasks_1（判据 kind=minichat，无 activityId）
    与 school_season（判据 kind=schoolseason，mini chat + activityId）——两者仅
    上报事件形状不同，链路（accept/claim/回读）完全一致。
    spec 由 MAPPING 提供，未映射的 mp 任务保守跳过。
    """
    spec = MAPPING.get(code) or {}
    kind = spec.get("kind") or "minichat"
    try:
        t = _mp_task_status(auth, code)
    except Exception as e:
        print(f"[task_runner] {uid8} {code}: mp list_tasks 失败: {e}")
        stats["fail"] += 1
        return
    if t is None:
        print(f"[task_runner] {uid8} {code}: mp 口径任务不存在，skip")
        stats["skip"] += 1
        return

    ast = t.get("accept_status")
    prog = t.get("progress") or {}
    cur = prog.get("current") or 0
    target = prog.get("target") or 1
    stats["total"] += 1

    if ast == "claimed":
        print(f"[task_runner] {uid8} {code}: query claimed({cur}/{target}) -> 已领，跳过")
        stats["already"] += 1
        return
    if ast == "completed" or cur >= target:
        if not opts.yes:
            print(f"[task_runner] {uid8} {code}: query completed({cur}/{target}) -> 可领(claim)，dry-run 跳过")
            stats["pending"] += 1
            return
        claim_one(auth, code, uid8, stats, opts.gap, mp=True)
        return

    if not opts.yes:
        print(f"[task_runner] {uid8} {code}: query {ast}({cur}/{target}) -> 可点亮({kind})，dry-run 跳过")
        stats["pending"] += 1
        return

    # 1) accept（mp 头；缺头实测 task not found）
    if ast == "not_accepted":
        st, r = tc.do_post(auth, tc.chat_base(auth), tc.PATH_ACCEPT_TASKS,
                           {"task_codes": [code]}, headers=MP_PLATFORM_HEADER)
        res = (((r.get("data") or {}).get("results") or [{}])[0].get("status")
               if isinstance(r, dict) else r)
        print(f"[task_runner] {uid8} {code}: accept {st} {res}")
        time.sleep(opts.gap)
        if st != 200 or res != "accepted":
            stats["fail"] += 1
            return

    # 2) 判据上报：mini 指纹 chat_request_send（minichat 无 activityId /
    #    schoolseason 带 activityId），走 codebuddy.cn 域（school.report_events）
    need = max(1, target - cur)
    for i in range(need):
        ev = build_event(auth, kind, "", {}, i)
        st, r = school.report_events(auth, [ev])
        sc = r.get("code") if isinstance(r, dict) else r
        print(f"[task_runner] {uid8} {code}: report {i + 1}/{need} {st} code={sc} (mini growth)")
        time.sleep(opts.gap)

    time.sleep(2.0)  # 服务端归账留时

    # 3) 回读 + claim（mp 头）
    t2 = _mp_task_status(auth, code) or t
    ast2 = t2.get("accept_status")
    prog2 = t2.get("progress") or {}
    cur2 = prog2.get("current", 0)
    print(f"[task_runner] {uid8} {code}: query re-read {cur2}/{prog2.get('target', target)} accept_status={ast2}")
    if ast2 == "claimed":
        print(f"[task_runner] {uid8} {code}: {ast} -> claimed（本轮已入账）")
        stats["already"] += 1
        return
    if ast2 == "completed" or cur2 >= prog2.get("target", target):
        claim_one(auth, code, uid8, stats, opts.gap, mp=True)
        return
    print(f"[task_runner] {uid8} {code}: report 未达 target（{cur2}/{prog2.get('target', target)}），WARN 待下次")
    stats["pending"] += 1


# --------------------------------------------------------------------------
# 单任务处理
# --------------------------------------------------------------------------
def process_task(auth, code, t, opts, stats):
    uid8 = auth["uid"][:8]
    spec = MAPPING.get(code)
    if spec is None:
        print(f"[task_runner] {uid8} {code}: query 非映射任务，skip")
        stats["total"] += 1
        stats["skip"] += 1
        return
    ast = t.get("accept_status")
    prog = t.get("progress") or {}
    cur = prog.get("current") or 0
    target = prog.get("target") or spec.get("target") or 1
    stats["total"] += 1

    # 不可伪造任务：跳过并如实标注
    if spec.get("unforgeable"):
        print(f"[task_runner] {uid8} {code}: query {ast}({cur}/{target}) -> 不可伪造({spec['reason']})，skip")
        stats["skip"] += 1
        return

    # 只领奖 模式：对已满足/已领任务执行 claim（已领->already_claimed 幂等），
    # 未满足任务跳过（服务端对未完成任务返回 task not completed，不强行）。
    if opts.only_claim:
        if ast != "claimed" and (cur < target and ast != "completed"):
            print(f"[task_runner] {uid8} {code}: query {ast}({cur}/{target}) -> 未 completed，only_claim 跳过")
            stats["pending"] += 1
            return
        if not opts.yes:
            print(f"[task_runner] {uid8} {code}: query {ast}({cur}/{target}) -> only_claim dry-run 跳过")
            stats["pending"] += 1
            return
        claim_one(auth, code, uid8, stats, opts.gap)
        return

    # 已领：跳过（除 only_claim 外不重复领）
    if ast == "claimed":
        print(f"[task_runner] {uid8} {code}: query claimed({cur}/{target}) -> 已领，跳过")
        stats["already"] += 1
        return

    claimed_or_done = (cur >= target or ast == "completed")

    # 已满足：走 claim
    if claimed_or_done:
        if not opts.yes:
            print(f"[task_runner] {uid8} {code}: query completed({cur}/{target}) -> 可领(claim)，dry-run 跳过")
            stats["pending"] += 1
            return
        claim_one(auth, code, uid8, stats, opts.gap)
        return

    # black_cat 特殊：时段敏感
    if code == "black_cat":
        in_window = within_night_window()
        if not in_window:
            print(f"[task_runner] {uid8} {code}: query in_progress({cur}/{target}) -> 非夜猫窗口(23-08 CST)，skip pending")
            stats["pending"] += 1
            return
        if not opts.yes:
            print(f"[task_runner] {uid8} {code}: query in_progress({cur}/{target}) -> 夜猫窗口内，可补 1 次，dry-run 跳过")
            stats["pending"] += 1
            return
        light_up(auth, code, spec, cur, target, uid8, opts, stats, cap=1)
        return

    # 可点亮：计算需要上报次数（按 target 补齐）
    need = max(0, target - cur)
    if need <= 0:
        if not opts.yes:
            print(f"[task_runner] {uid8} {code}: query {ast}({cur}/{target}) -> 可领(claim)，dry-run 跳过")
            stats["already"] += 1
            return
        claim_one(auth, code, uid8, stats, opts.gap)
        return

    if not opts.yes:
        ids = ids_for(spec["kind"], auth, need, offset=cur)
        shown = [i for i, _ in ids][:6]
        print(f"[task_runner] {uid8} {code}: query {ast}({cur}/{target}) -> 可点亮 need={need} "
              f"id源={spec['src']} ids={shown}，dry-run 跳过")
        stats["pending"] += 1
        return

    light_up(auth, code, spec, cur, target, uid8, opts, stats)


def _accept_with_verify(auth, code, uid8, gap) -> bool:
    """accept 并验证登记生效：读 results[].status（而非只看 HTTP 200/msg——
    实测上游存在 200 + msg=OK 但 status 非 accepted、accept 未真正登记的形态，
    此时上报事件全部不归账，任务永远点不亮）。

    返回 True=accept 已生效（本轮或此前）；False=重试后仍未生效。
    判定以回读 accept_status 为准（响应 status 只是初筛）。
    """
    for attempt in (1, 2):
        st, r = tc.accept_tasks(auth, [code])
        status = ""
        if isinstance(r, dict):
            results = ((r.get("data") or {}).get("results") or [])
            status = (results[0].get("status") or "") if results else (r.get("msg") or "")
        else:
            status = str(r)
        # 回读确认登记生效（响应可能说 accepted 但服务端未落账）
        t = tc.task_status(auth, code) or {}
        ast = t.get("accept_status") or "not_accepted"
        ok = (st == 200 and status == "accepted" and ast != "not_accepted")
        print(f"[task_runner] {uid8} {code}: accept 尝试{attempt} {st} status={status} "
              f"回读={ast}{' -> 生效' if ok else ''}")
        if ok:
            return True
        time.sleep(gap)
    return False


def light_up(auth, code, spec, cur, target, uid8, opts, stats, cap=0):
    """accept(若未接，带登记验证与重试) → 按 target 补齐上报 → 回读 → 已满则 claim。"""
    ast = tc.task_status(auth, code)
    ast = ast.get("accept_status") if ast else "not_accepted"
    if ast == "not_accepted":
        if not _accept_with_verify(auth, code, uid8, opts.gap):
            print(f"[task_runner] {uid8} {code}: accept 未登记生效，本轮跳过待下次")
            stats["fail"] += 1
            return

    t_now = tc.task_status(auth, code) or {}
    prog = t_now.get("progress") or {}
    cur = prog.get("current") or cur
    tgt = prog.get("target") or target
    need = max(0, tgt - cur)
    if cap > 0:
        need = min(need, cap)
    if need <= 0:
        print(f"[task_runner] {uid8} {code}: report 无需上报（{cur}/{tgt}）")
    elif spec["kind"] == "buddyfirst":
        # 领养第一只 Buddy：report（前置解锁）→ agreement → buddy/first
        # （fork adoptBuddy。buddy/first 直接发积分 +300c+8e，任务状态随之 completed；
        #   门槛未达时服务端返回 HTTP 400 "first_buddy task not completed yet"，如实标注）。
        st_r, sc = tc.report_activity(auth, count=1, gap=opts.gap)[0]
        print(f"[task_runner] {uid8} {code}: report {st_r} code={sc} 前置解锁")
        time.sleep(2.0)  # 服务端事件归账留时
        st_a, ag = tc.do_post(auth, tc.chat_base(auth), tc.PATH_BUDDY_AGREEMENT, {"agree": True})
        print(f"[task_runner] {uid8} {code}: buddy/agreement -> {st_a} code={(ag.get('code') if isinstance(ag, dict) else ag)}")
        time.sleep(opts.gap)
        st_b, bf = tc.do_post(auth, tc.chat_base(auth), tc.PATH_BUDDY_FIRST, {})
        msg = bf.get("msg") if isinstance(bf, dict) else bf
        data = bf.get("data") or {} if isinstance(bf, dict) else {}
        cred = data.get("credit") or 0 if isinstance(data, dict) else 0
        energy = data.get("energy") or 0
        print(f"[task_runner] {uid8} {code}: buddy/first -> {st_b} {msg} credit=+{cred} energy=+{energy}")
        # 领养直给积分即任务完成：直接计入入账汇总（后续 claim 为 already_claimed）
        if st_b == 200 and (isinstance(bf, dict) and bf.get("code") in (0, None)):
            stats["ok"] += 1
            stats["credit"] += cred
            stats["energy"] += energy
        time.sleep(2.0)
    elif spec["kind"] == "richmeow":
        # 桌面指纹 6 连对话事件链（fork DesktopChatSequence），单组即一次完整对话
        for i in range(need):
            ms = int(time.time() * 1000)
            conv = f"wb-run-rm-{ms}-{i}"
            req = f"wb-run-rm-req-{ms}-{i}"
            msg = f"req-{ms}-{i}-user"
            events = desktop_chat_sequence(auth, conv, req, msg, "fast-model", "fast-model")
            st_r, sc = report_desktop_events(auth, events)
            print(f"[task_runner] {uid8} {code}: report {i + 1}/{need} {st_r} code={sc} "
                  f"deskchain conv={conv}")
            if i < need - 1:
                time.sleep(opts.gap)
        time.sleep(2.0)  # 服务端归账可能异步
    elif spec["kind"] == "buddy5":
        # 五连「进入 Buddy 应用」事件（fork DesktopBuddyAppSequence，两账号实测点亮）；
        # 上报失败（非 200 / code!=0）降级为单发 buddyapp_enter_click（旧形状兜底）。
        buddy_id, buddy_name = BUDDY_APPS.get(code, ("", "Buddy"))
        for i in range(need):
            events = desktop_buddy5_sequence(auth, buddy_id, buddy_name)
            st_r, sc = report_desktop_events(auth, events)
            if st_r != 200 or sc != 0:
                now = int(time.time() * 1000)
                single = {"eventCode": "buddyapp_enter_click", "timestamp": now,
                          "reportDelay": 0, "elementId": buddy_id,
                          "elementName": buddy_name, "position": 1,
                          "isFirstPage": "0", "userId": auth["uid"]}
                st2, sc2 = report_desktop_events(auth, [single])
                print(f"[task_runner] {uid8} {code}: report {i + 1}/{need} {st_r} code={sc} "
                      f"buddy5 失败 -> 降级单发 {st2} code={sc2}")
            else:
                print(f"[task_runner] {uid8} {code}: report {i + 1}/{need} {st_r} code={sc} "
                      f"buddy5 id={buddy_id}")
            if i < need - 1:
                time.sleep(opts.gap)
        time.sleep(2.0)  # 服务端归账可能异步
    elif spec["kind"] == "library":
        # web 域 web_element_click（fork ReportWebEvent），base 为 www.workbuddy.cn
        for i in range(need):
            st_r, sc = report_web_event(auth, "web_element_click", LIBRARY_DOC_URL,
                                        "library_doc_intro_click", "WorkBuddy资料库介绍")
            print(f"[task_runner] {uid8} {code}: report {i + 1}/{need} {st_r} code={sc} "
                  f"web_element_click")
            if i < need - 1:
                time.sleep(opts.gap)
        time.sleep(2.0)  # 服务端归账可能异步
    else:
        ids = ids_for(spec["kind"], auth, need, offset=cur)
        if not ids:
            print(f"[task_runner] {uid8} {code}: report 无可用对象 id，skip")
            stats["fail"] += 1
            return
        for i, (obj_id, m) in enumerate(ids):
            ev = build_event(auth, spec["kind"], obj_id, m, i)
            st_r, r_r = tc.do_post(auth, tc.billing_base(auth), tc.PATH_REPORT, [ev])
            sc = r_r.get("code") if isinstance(r_r, dict) else r_r
            print(f"[task_runner] {uid8} {code}: report {i + 1}/{need} {st_r} code={sc} id={obj_id}")
            if i < len(ids) - 1:
                time.sleep(opts.gap)
        time.sleep(2.0)  # 服务端归账可能异步

    # 回读确认
    t2 = tc.task_status(auth, code)
    prog2 = (t2.get("progress") or {}) if t2 else {}
    cur2 = prog2.get("current", 0)
    ast2 = t2.get("accept_status") if t2 else "?"
    print(f"[task_runner] {uid8} {code}: query re-read {cur2}/{prog2.get('target', tgt)} accept_status={ast2}")
    if ast2 != "claimed" and cur2 >= prog2.get("target", tgt):
        claim_one(auth, code, uid8, stats, opts.gap)
    elif ast2 != "claimed":
        # 未点亮：如实标注，不强行刷
        print(f"[task_runner] {uid8} {code}: report 未达 target（{cur2}/{prog2.get('target', tgt)}），WARN 待下次")


# --------------------------------------------------------------------------
# 账号级流程
# --------------------------------------------------------------------------
def process_account(auth, opts, stats):
    uid8 = auth["uid"][:8]
    print(f"== {uid8} ({auth['nick']}) ==")
    # 只读 oracle：能量余额（上下文参考，dry-run/REAL 都显示）
    try:
        st_e, r_e = tc.do_get(auth, tc.chat_base(auth), PATH_ENERGY)
        if st_e == 200 and isinstance(r_e, dict) and r_e.get("code") == 0:
            bal = (r_e.get("data") or {}).get("balance", "?")
            print(f"[task_runner] {uid8} query energy balance={bal}")
    except Exception:
        pass
    try:
        tasks = tc.list_tasks(auth)
    except Exception as e:
        print(f"ERR: [task_runner] {uid8} query list_tasks 失败: {e}")
        stats["fail"] += 1
        return

    by_code = {t.get("task_code"): t for t in tasks}
    # growth 映射里带 school 段的 codes：growth 任务列表无此 code（school 域独立任务空间），
    # 由 process_school_tasks 专段处理，不进 process_task（growth claim 端点对 school 任务 400）。
    # minichat 同理：mp 限定任务在默认口径列表不出现，由 process_minichat_task 专段处理。
    school_in_map = [c for c in MAPPING if MAPPING[c].get("kind") == "school"]
    minichat_in_map = [c for c in MAPPING if MAPPING[c].get("kind") in ("minichat", "schoolseason")]
    special = set(school_in_map) | set(minichat_in_map)
    if opts.only_codes:
        codes = [c for c in opts.only_codes if c not in special]
        process_school = bool(set(opts.only_codes) & set(school_in_map))
        process_minichat = bool(set(opts.only_codes) & set(minichat_in_map))
    else:
        codes = [c for c in MAPPING if MAPPING[c].get("kind") not in ("school", "minichat", "schoolseason")]
        process_school = True
        process_minichat = True
        # 未在映射表但存在于任务列表的（如 Expert_Philanthropy）——只计数展示
        for t in tasks:
            c = t.get("task_code")
            if c and c not in MAPPING:
                ast = t.get("accept_status")
                if ast == "claimed":
                    print(f"[task_runner] {uid8} {c}: query claimed -> 已领，跳过(非映射任务)")
                    stats["total"] += 1
                    stats["already"] += 1
                else:
                    print(f"[task_runner] {uid8} {c}: query {ast} -> 非映射任务，skip")
                    stats["total"] += 1
                    stats["skip"] += 1

    for code in codes:
        t = by_code.get(code)
        if t is None:
            print(f"[task_runner] {uid8} {code}: query 任务不存在")
            stats["total"] += 1
            stats["skip"] += 1
            continue
        process_task(auth, code, t, opts, stats)

    # 小程序成长任务段（mp 口径）：已完成/可领状态下 only_claim 模式仍可入账（claim 幂等）；
    # 未完成则点亮判据属写操作，与 school 段同规则只在非 only_claim 模式触发。
    if process_minichat:
        if opts.only_claim and not opts.yes:
            pass  # only_claim+dry-run：无写操作，专段内部自行按 dry-run 跳过
        else:
            # --only 语义：指定了 mp code 时只跑指定的（与 growth/school 段一致）
            mp_codes = ([c for c in opts.only_codes if c in minichat_in_map]
                        if opts.only_codes else minichat_in_map)
            for mp_code in mp_codes:
                try:
                    t = _mp_task_status(auth, mp_code)
                    ast = (t or {}).get("accept_status")
                    if opts.only_claim and ast not in ("completed", "claimed"):
                        # only_claim 只入账已完成任务；未完成不点亮
                        print(f"[task_runner] {uid8} {mp_code}: only_claim 跳过（未 completed）")
                    else:
                        process_minichat_task(auth, mp_code, opts, stats, uid8)
                except Exception as e:
                    print(f"[task_runner] {uid8} {mp_code}: mp 查询失败: {e}")
                    stats["fail"] += 1

    # school 段（小程序开学季）：growth 任务循环之后执行，判据/端点全复用 school 模块。
    # --only-claim 语义下跳过：school 无「已 completed 未领」的 only_claim 快路径，
    # 点亮判据是写操作，不应在只领奖模式下触发（实测曾由此误触发写，见报告）。
    if process_school and not opts.only_claim:
        process_school_tasks(auth, opts, stats, uid8)


# --------------------------------------------------------------------------
# 汇总
# --------------------------------------------------------------------------
def print_summary(stats):
    print(f"task_runner done: accounts={stats['accounts']} total={stats['total']} "
          f"ok={stats['ok']} already={stats['already']} skipped={stats['skip']} "
          f"pending={stats['pending']} fail={stats['fail']} "
          f"credit=+{stats['credit']} energy=+{stats['energy']}")


def main():
    ap = argparse.ArgumentParser(description="成长任务一体机：查询→完成(点亮)→领取")
    ap.add_argument("accounts", nargs="+", help="uid 前缀（可多个）或 ALL")
    ap.add_argument("--yes", action="store_true", help="真实执行写操作（默认 dry-run）")
    ap.add_argument("--only", action="append", default=None, dest="only_codes",
                    help="只处理指定 task_code（可多次）")
    ap.add_argument("--only-claim", action="store_true",
                    help="只领奖不点亮（已完成任务直接 claim，已领幂等）")
    ap.add_argument("--gap", type=float, default=1.0, help="动作间隔秒数（默认 1.0）")
    a = ap.parse_args()

    if a.gap < 1.0:
        a.gap = 1.0  # 写操作间隔 ≥1s

    stats = {"accounts": 0, "total": 0, "ok": 0, "already": 0,
             "skip": 0, "pending": 0, "fail": 0, "credit": 0, "energy": 0}

    prefixes = []
    for acc in a.accounts:
        if acc.upper() == "ALL":
            # tc.iter_auth_files 已排除 zcode 等非 WorkBuddy 来源（扁平形落盘、
            # 无 CN 任务中心），避免 load_auth 抛错中断整批。
            prefixes += [os.path.basename(p)[10:18] for p in tc.iter_auth_files()]
        else:
            prefixes.append(acc)
    # 去重保序
    seen, prefixes2 = set(), []
    for p in prefixes:
        if p not in seen:
            seen.add(p)
            prefixes2.append(p)

    mode = "REAL" if a.yes else "DRY-RUN"
    print(f"mode={mode} accounts={prefixes2} only={a.only_codes or 'all'} "
          f"only_claim={a.only_claim} gap={a.gap}")
    if a.yes and len(prefixes2) > 2 and not a.only_codes and not a.only_claim:
        print("WARN: 全量批量 + --yes 未限定 --only，注意 54 号批量——请确认 Hermes 决策后再跑")

    for p in prefixes2:
        try:
            c = tc.load_auth(p)
        except SystemExit as e:
            print(f"ERR: {e}")
            continue
        stats["accounts"] += 1
        # global realm 不适用 CN 任务中心：明确跳过、不发起任何请求（P2 门控结论）。
        if tc.auth_is_global(c):
            uid8 = (c.get("uid") or "")[:8] or "?"
            print(f"[skip] {uid8} global realm 不适用 CN 任务")
            stats["skip"] += 1
            continue
        process_account(c, a, stats)

    print_summary(stats)


if __name__ == "__main__":
    main()