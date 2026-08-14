# -*- coding: utf-8 -*-
"""把 New-API-用户指南/README.md 转为静态 HTML(web/public/docs/index.html)"""
import shutil
from pathlib import Path

import markdown

SRC = Path(r"D:\中转站\New-API-用户指南")
OUT = Path(r"D:\中转站\new-api-src\web\public\docs")

md_text = (SRC / "README.md").read_text(encoding="utf-8")
body = markdown.markdown(
    md_text,
    extensions=["tables", "fenced_code", "toc"],
    output_format="html5",
)

CSS = """
:root{color-scheme:light}
*{box-sizing:border-box}
body{margin:0;font-family:-apple-system,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;line-height:1.75;color:#1f2328;background:#fff}
.container{max-width:860px;margin:0 auto;padding:40px 24px 80px}
h1{font-size:2em;border-bottom:1px solid #e6e6e6;padding-bottom:.3em;margin-top:1.5em}
h2{font-size:1.5em;border-bottom:1px solid #e6e6e6;padding-bottom:.3em;margin-top:1.4em}
h3{font-size:1.2em;margin-top:1.2em}
a{color:#0969da;text-decoration:none}
a:hover{text-decoration:underline}
code{background:#f3f4f6;border-radius:4px;padding:.15em .4em;font-family:"SFMono-Regular",Consolas,"Courier New",monospace;font-size:.9em}
pre{background:#f6f8fa;border:1px solid #e6e6e6;border-radius:8px;padding:14px 16px;overflow:auto}
pre code{background:none;padding:0}
img{max-width:100%;border:1px solid #e6e6e6;border-radius:8px;display:block;margin:12px auto}
table{border-collapse:collapse;width:100%;margin:12px 0;display:block;overflow-x:auto}
th,td{border:1px solid #d0d7de;padding:8px 12px;text-align:left}
th{background:#f6f8fa;font-weight:600}
blockquote{border-left:4px solid #d0d7de;margin:12px 0;padding:4px 14px;color:#57606a;background:#f6f8fa;border-radius:0 8px 8px 0}
hr{border:none;border-top:1px solid #e6e6e6;margin:2em 0}
.toc{background:#f6f8fa;border:1px solid #e6e6e6;border-radius:8px;padding:14px 20px;margin-bottom:8px}
.toc ul{list-style:none;padding-left:0;margin:6px 0}
.toc li{margin:2px 0}
.toc a{color:#0969da}
.crumb{color:#57606a;font-size:.85em;margin-bottom:8px}
"""

html = f"""<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>New API 用户指南</title>
<style>{CSS}</style>
</head>
<body>
<div class="container">
<article>
{body}
</article>
</div>
</body>
</html>
"""

OUT.mkdir(parents=True, exist_ok=True)
(OUT / "index.html").write_text(html, encoding="utf-8")
# 复制图片资源,保持目录结构
shutil.copytree(SRC / "assets", OUT / "assets", dirs_exist_ok=True)
print("OK ->", OUT / "index.html")
print("assets ->", OUT / "assets")
