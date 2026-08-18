# -*- coding: utf-8 -*-
"""把 API-用户指南/README.md 转为静态 HTML(web/public/docs/index.html)。"""
import re
import shutil
from pathlib import Path

import markdown

WORKSPACE_ROOT = Path(__file__).resolve().parents[3]
SRC = WORKSPACE_ROOT / "API-用户指南"
OUT = Path(__file__).resolve().parents[1] / "public" / "docs"

md_text = (SRC / "README.md").read_text(encoding="utf-8")


def slugify(value, separator):
    slug = re.sub(r"[^\w\u4e00-\u9fff-]+", separator, value.lower())
    return re.sub(f"{re.escape(separator)}+", separator, slug).strip(separator)


renderer = markdown.Markdown(
    extensions=["tables", "fenced_code", "toc"],
    extension_configs={"toc": {"slugify": slugify}},
    output_format="html5",
)
body = renderer.convert(md_text)

toc_match = re.search(
    r'<h2 id="目录">目录</h2>\s*(<ol>.*?</ol>)\s*<hr>', body, re.DOTALL
)
if toc_match is None:
    raise RuntimeError("README.md 中未找到目录")

toc = toc_match.group(1)
body = body[: toc_match.start()] + body[toc_match.end() :]

CSS = """
:root{color-scheme:light;--bg:#f5f7fb;--surface:#ffffff;--surface-soft:#f8fafc;--text:#172033;--muted:#667085;--line:#e4e9f1;--primary:#2563eb;--primary-soft:#eff6ff;--code:#0f172a}
*{box-sizing:border-box}
html{scroll-behavior:smooth}
body{margin:0;font-family:Inter,-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;line-height:1.8;color:var(--text);background:radial-gradient(circle at 50% 0,#fff 0,#f8faff 32%,var(--bg) 70%);-webkit-font-smoothing:antialiased}
.site-header{position:sticky;top:0;z-index:20;height:64px;border-bottom:1px solid rgba(228,233,241,.9);background:rgba(255,255,255,.82);backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px)}
.site-header-inner{width:min(1380px,100%);height:100%;margin:0 auto;padding:0 28px;display:flex;align-items:center;justify-content:space-between}
.brand{display:inline-flex;align-items:center;gap:11px;color:var(--text);font-size:16px;font-weight:700;letter-spacing:.01em}
.brand:hover{text-decoration:none}
.brand-mark{display:block;width:36px;height:36px}
.header-tag{padding:5px 10px;border:1px solid var(--line);border-radius:999px;color:var(--muted);background:#fff;font-size:12px;font-weight:600}
.docs-layout{width:min(1380px,100%);margin:0 auto;padding:34px 28px 96px;display:grid;grid-template-columns:240px minmax(0,900px);gap:44px;justify-content:center;align-items:start}
.container{min-width:0}
.doc-card{padding:52px 64px 72px;border:1px solid rgba(220,226,236,.92);border-radius:20px;background:rgba(255,255,255,.96);box-shadow:0 16px 45px rgba(23,32,51,.07)}
h1,h2,h3{color:var(--text);scroll-margin-top:92px;line-height:1.35;letter-spacing:-.025em}
h1{margin:2.2em 0 .8em;padding-bottom:.42em;border-bottom:1px solid var(--line);font-size:30px}
h1:first-child{margin:0 0 1.35em;padding-bottom:.55em;font-size:40px;letter-spacing:-.04em;background:linear-gradient(110deg,#172033 10%,#2563eb 80%);background-clip:text;-webkit-background-clip:text;color:transparent}
h2{margin:2em 0 .65em;font-size:23px}
h3{margin:1.65em 0 .55em;font-size:18px}
p{margin:.75em 0;color:#344054}
a{color:var(--primary);text-decoration:none;text-underline-offset:3px}
a:hover{text-decoration:underline}
code{padding:.18em .45em;border:1px solid #e5eaf2;border-radius:6px;color:#c026d3;background:#f8fafc;font-family:"SFMono-Regular",Consolas,"Courier New",monospace;font-size:.88em}
pre{margin:1.2em 0;padding:18px 20px;overflow:auto;border:1px solid #1e293b;border-radius:12px;color:#e2e8f0;background:var(--code);box-shadow:inset 0 1px rgba(255,255,255,.05)}
pre code{padding:0;border:0;color:inherit;background:none}
img{display:block;max-width:100%;margin:20px auto 28px;border:1px solid var(--line);border-radius:14px;background:#fff;box-shadow:0 10px 30px rgba(15,23,42,.09)}
ol,ul{padding-left:1.45em}
li{padding-left:.2em;margin:.34em 0;color:#344054}
table{display:table;width:100%;margin:22px 0;border-collapse:separate;border-spacing:0;overflow:hidden;border:1px solid var(--line);border-radius:12px;font-size:14px}
th,td{padding:11px 14px;border-right:1px solid var(--line);border-bottom:1px solid var(--line);text-align:left}
th:last-child,td:last-child{border-right:0}
tr:last-child td{border-bottom:0}
th{color:#344054;background:var(--surface-soft);font-weight:650}
blockquote{margin:20px 0;padding:14px 18px;border:1px solid #bfdbfe;border-left:4px solid var(--primary);border-radius:8px 12px 12px 8px;color:#344054;background:var(--primary-soft)}
blockquote p{margin:0}
hr{height:1px;margin:42px 0;border:0;background:linear-gradient(90deg,transparent,var(--line) 12%,var(--line) 88%,transparent)}
.toc{position:sticky;top:88px;max-height:calc(100vh - 112px);overflow-y:auto;padding:18px 12px 20px;border:1px solid rgba(220,226,236,.92);border-radius:16px;background:rgba(255,255,255,.82);box-shadow:0 10px 30px rgba(23,32,51,.045);backdrop-filter:blur(10px);scrollbar-width:thin}
.toc-eyebrow{padding:0 12px 2px;color:#98a2b3;font-size:11px;font-weight:700;letter-spacing:.12em;text-transform:uppercase}
.toc-title{padding:0 12px 12px;color:var(--text);font-size:17px;font-weight:750}
.toc ol{margin:0;padding:0;list-style:none;counter-reset:toc}
.toc li{position:relative;margin:2px 0;padding:0;counter-increment:toc}
.toc a{display:flex;align-items:center;gap:9px;padding:8px 11px;border-radius:9px;color:#5d6b82;font-size:14px;font-weight:500;line-height:1.45;transition:color .18s ease,background .18s ease,transform .18s ease}
.toc a::before{content:counter(toc,decimal-leading-zero);color:#a3adbd;font-size:10px;font-variant-numeric:tabular-nums;letter-spacing:.04em}
.toc a:hover{color:var(--primary);background:#f4f7fc;text-decoration:none;transform:translateX(2px)}
.toc a.active{color:#1d4ed8;background:var(--primary-soft);font-weight:650}
.toc a.active::before{color:#3b82f6}
@media (max-width:980px){.docs-layout{grid-template-columns:minmax(0,900px);gap:22px;padding-top:24px}.toc{position:static;max-height:none}.toc ol{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:2px 8px}.container{width:100%}}
@media (max-width:640px){.site-header-inner{padding:0 18px}.header-tag{display:none}.docs-layout{padding:16px 12px 60px}.doc-card{padding:30px 22px 48px;border-radius:15px}h1:first-child{font-size:32px}h1{font-size:26px}h2{font-size:21px}.toc ol{grid-template-columns:1fr}table{display:block;overflow-x:auto;white-space:nowrap}}
"""

SCRIPT = """
const links = [...document.querySelectorAll('.toc a')];
const sections = links
  .map((link) => document.getElementById(decodeURIComponent(link.hash.slice(1))))
  .filter(Boolean);

function updateActiveLink() {
  const marker = window.scrollY + 150;
  let active = sections[0];
  for (const section of sections) {
    if (section.offsetTop <= marker) active = section;
  }
  for (const link of links) {
    const isActive = decodeURIComponent(link.hash.slice(1)) === active?.id;
    link.classList.toggle('active', isActive);
    if (isActive) link.setAttribute('aria-current', 'location');
    else link.removeAttribute('aria-current');
  }
}

window.addEventListener('scroll', updateActiveLink, { passive: true });
window.addEventListener('hashchange', updateActiveLink);
updateActiveLink();
"""

html = f"""<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>JadeRoute 用户指南</title>
<link rel="icon" type="image/svg+xml" href="/brand/jaderoute-mark.svg">
<style>{CSS}</style>
</head>
<body>
<header class="site-header">
<div class="site-header-inner">
<a class="brand" href="/docs/" aria-label="JadeRoute 用户指南首页"><img class="brand-mark" src="/brand/jaderoute-mark.svg" alt=""><span>JadeRoute 用户指南</span></a>
<span class="header-tag">开发者文档</span>
</div>
</header>
<div class="docs-layout">
<aside class="toc" aria-label="文档目录">
<div class="toc-eyebrow">Contents</div>
<div class="toc-title">目录</div>
{toc}
</aside>
<main class="container">
<article class="doc-card">
{body}
</article>
</main>
</div>
<script>{SCRIPT}</script>
</body>
</html>
"""

OUT.mkdir(parents=True, exist_ok=True)
(OUT / "index.html").write_text(html, encoding="utf-8")
# 复制图片资源,保持目录结构
shutil.copytree(SRC / "assets", OUT / "assets", dirs_exist_ok=True)
print("OK ->", OUT / "index.html")
print("assets ->", OUT / "assets")
