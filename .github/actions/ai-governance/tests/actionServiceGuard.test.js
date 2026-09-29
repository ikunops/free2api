// R3/C3 守卫测试：所有 actionService.* 调用点引用的方法必须真实存在。
// 此前 closeAndLock 被调用但从未定义（C3）—— BASIC issue 路径 TypeError 后被吞，
// 带 null 分类流入治理层。本测试静态枚举调用点，方法缺失即失败，杀死整类 bug。

const fs = require('fs');
const path = require('path');

const IssueActionService = require('../src/services/issueActionService');
const PrActionService = require('../src/services/prActionService');

const SRC_DIR = path.join(__dirname, '..', 'src');

function walk(dir) {
  const out = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...walk(full));
    } else {
      out.push(full);
    }
  }
  return out;
}

describe('actionService 方法存在性守卫（R3/C3）', () => {
  test('src 中每个 actionService.<method>( 调用点都能在对应服务类上找到定义', () => {
    const issueMethods = Object.getOwnPropertyNames(IssueActionService.prototype);
    const prMethods = Object.getOwnPropertyNames(PrActionService.prototype);

    const missing = [];
    const files = walk(SRC_DIR).filter(f => f.endsWith('.js'));
    for (const file of files) {
      const content = fs.readFileSync(file, 'utf8');
      // this.actionService.<method>( —— 工作流服务持有 actionService 实例
      for (const m of content.matchAll(/actionService\.([A-Za-z0-9_]+)\s*\(/g)) {
        const method = m[1];
        if (!issueMethods.includes(method) && !prMethods.includes(method)) {
          missing.push(`${path.relative(SRC_DIR, file)}: actionService.${method}`);
        }
      }
    }
    expect(missing).toEqual([]);
  });

  test('closeAndLock 存在（FIX-A 回归锚点：C3 的崩溃点）', () => {
    expect(typeof IssueActionService.prototype.closeAndLock).toBe('function');
  });
});
