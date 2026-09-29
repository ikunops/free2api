const fs = require('fs');
const path = require('path');
const baseConfig = require('../config.json');

// 契约测试（C14/C15/C17）：config.json 与两个 locale 文件的键必须一一对应，
// 且代码中引用的每个 responses/logging/prompts 键都必须真实存在。
// 这类测试专门杀死「mock 比真实实现更丰富」和「合并丢键」两类 bug。

const LOCALES_DIR = path.join(__dirname, '..', 'locales');

function loadLocale(name) {
  return JSON.parse(fs.readFileSync(path.join(LOCALES_DIR, name), 'utf8'));
}

describe('locale/config key consistency', () => {
  const zh = loadLocale('zh-CN.json');
  const en = loadLocale('en.json');

  test('config.responses 键与 zh-CN / en 两个 locale 完全一致（漂移即失败）', () => {
    const configKeys = new Set(Object.keys(baseConfig.responses));
    const zhKeys = new Set(Object.keys(zh.responses));
    const enKeys = new Set(Object.keys(en.responses));

    expect([...zhKeys].filter(k => !configKeys.has(k))).toEqual([]);
    expect([...configKeys].filter(k => !zhKeys.has(k))).toEqual([]);
    expect([...enKeys].filter(k => !configKeys.has(k))).toEqual([]);
    expect([...configKeys].filter(k => !enKeys.has(k))).toEqual([]);
  });

  test('src 与 tests 中引用的每个 config.logging.* 键都存在于 config.json', () => {
    const srcDir = path.join(__dirname, '..', 'src');
    const loggingKeys = new Set(Object.keys(baseConfig.logging));
    const missing = [];

    const files = walk(srcDir).filter(f => f.endsWith('.js'));
    for (const file of files) {
      const content = fs.readFileSync(file, 'utf8');
      // config.logging.xxx / this.config.logging.xxx / configForX.logging.xxx
      const matches = content.matchAll(/(?:config|this\.config|configForDraft)\.logging\.([A-Za-z0-9_]+)/g);
      for (const m of matches) {
        if (!loggingKeys.has(m[1])) {
          missing.push(`${path.relative(srcDir, file)}: logging.${m[1]}`);
        }
      }
    }

    expect(missing).toEqual([]);
  });

  test('src 中引用的每个 config.responses.* 键都存在于 config.json', () => {
    const srcDir = path.join(__dirname, '..', 'src');
    const responseKeys = new Set(Object.keys(baseConfig.responses));
    const missing = [];

    const files = walk(srcDir).filter(f => f.endsWith('.js'));
    for (const file of files) {
      const content = fs.readFileSync(file, 'utf8');
      const matches = content.matchAll(/(?:config|this\.config)\.responses\.([A-Za-z0-9_]+)/g);
      for (const m of matches) {
        if (!responseKeys.has(m[1])) {
          missing.push(`${path.relative(srcDir, file)}: responses.${m[1]}`);
        }
      }
      // render('key') / config.responses[responseKey] 这类模板调用在测试层覆盖
      const renderMatches = content.matchAll(/this\.render\('([A-Za-z0-9_]+)'/g);
      for (const m of renderMatches) {
        if (!responseKeys.has(m[1])) {
          missing.push(`${path.relative(srcDir, file)}: render('${m[1]}')`);
        }
      }
    }

    expect(missing).toEqual([]);
  });

  test('src 中引用的每个 config.prompts.* 键都存在于 config.json', () => {
    const srcDir = path.join(__dirname, '..', 'src');
    const promptKeys = new Set(Object.keys(baseConfig.prompts));
    const missing = [];

    const files = walk(srcDir).filter(f => f.endsWith('.js'));
    for (const file of files) {
      const content = fs.readFileSync(file, 'utf8');
      const matches = content.matchAll(/(?:config|this\.config)\.prompts\.([A-Za-z0-9_]+)/g);
      for (const m of matches) {
        if (!promptKeys.has(m[1])) {
          missing.push(`${path.relative(srcDir, file)}: prompts.${m[1]}`);
        }
      }
    }

    expect(missing).toEqual([]);
  });

  test('issueProcessor / prHandler 使用的 responseKey / logKey 字面量都真实存在', () => {
    // closeIssueWithType / handleSpamIssue 等以字符串字面量引用键，静态扫描兜底
    const responseKeys = new Set(Object.keys(baseConfig.responses));
    const loggingKeys = new Set(Object.keys(baseConfig.logging));

    // prHandler / issueHandler / issueProcessor / workflowService 的字面量
    const literals = [
      'issue_spam', 'issue_spam_log',
      'issue_content_filtered', 'issue_content_filtered_log',
      'issue_basic', 'issue_basic_log',
      'pr_closed', 'pr_closed_log',
      'pr_content_filtered', 'pr_content_filtered_log',
      'pr_malicious', 'pr_malicious_log',
      'pr_trivial', 'pr_trivial_log',
      'issue_readme_covered', 'issue_readme_covered_log',
      'issue_unclear', 'issue_unclear_comment_failed',
      'governance_ai_fallback_comment', 'governance_pr_ai_fallback_log'
    ];
    const missing = literals.filter(k =>
      !(responseKeys.has(k) || loggingKeys.has(k))
    );
    expect(missing).toEqual([]);
  });

  test('R7 死键守卫：config.json 的 prompts/logging 无「定义了但全库零引用」的键（漂移即失败）', () => {
    const srcDir = path.join(__dirname, '..', 'src');
    const files = walk(srcDir).filter(f => f.endsWith('.js'));
    const allSrc = files.map(f => fs.readFileSync(f, 'utf8')).join('\n');
    // 其它测试文件共同构成「有人在用」的证据（测试引用也算引用）
    const testDir = path.join(__dirname);
    const allTests = walk(testDir)
      .filter(f => f.endsWith('.js') && !f.endsWith('localeConsistency.test.js'))
      .map(f => fs.readFileSync(f, 'utf8')).join('\n');
    const corpus = allSrc + '\n' + allTests;

    const dead = [];
    for (const key of Object.keys(baseConfig.prompts)) {
      if (!corpus.includes(key)) {
        dead.push(`prompts.${key}`);
      }
    }
    for (const key of Object.keys(baseConfig.logging)) {
      if (!corpus.includes(key)) {
        dead.push(`logging.${key}`);
      }
    }
    expect(dead).toEqual([]);
  });
});

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
