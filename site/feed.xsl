<?xml version="1.0" encoding="UTF-8"?>
<!-- feed.xml 在浏览器里直接打开时的样式:Atom 源本身是给阅读器的,人点进来也能看懂。 -->
<xsl:stylesheet version="1.0" xmlns:xsl="http://www.w3.org/1999/XSL/Transform" xmlns:atom="http://www.w3.org/2005/Atom" exclude-result-prefixes="atom">
  <xsl:output method="html" encoding="UTF-8" indent="yes"/>
  <xsl:template match="/">
    <html lang="en">
      <head>
        <meta charset="utf-8"/>
        <meta name="viewport" content="width=device-width, initial-scale=1"/>
        <title><xsl:value-of select="atom:feed/atom:title"/></title>
        <style>
          :root{color-scheme:light dark;--bg:#f6f7f9;--card:#fff;--text:#1a202c;--muted:#64748b;--border:#e2e8f0;--primary:#2563eb}
          @media(prefers-color-scheme:dark){:root{--bg:#0f1216;--card:#171b21;--text:#e6e9ef;--muted:#8b95a5;--border:#2a313b;--primary:#60a5fa}}
          body{margin:0;background:var(--bg);color:var(--text);font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",system-ui,sans-serif}
          .wrap{max-width:720px;margin:0 auto;padding:2rem 1rem 3rem}
          h1{font-size:1.35rem;margin:0 0 .3rem}
          .sub{color:var(--muted);margin:0 0 1.2rem}
          .note{background:var(--card);border:1px solid var(--border);border-radius:10px;padding:.7rem .9rem;font-size:.85rem;color:var(--muted);margin-bottom:1.4rem}
          .note code{font-size:.8rem;user-select:all}
          .entry{background:var(--card);border:1px solid var(--border);border-radius:12px;padding:.9rem 1rem;margin-bottom:.7rem}
          .entry h2{font-size:1.02rem;margin:0 0 .25rem}
          .entry a{color:var(--primary);text-decoration:none}
          .entry a:hover{text-decoration:underline}
          .date{font-size:.78rem;color:var(--muted)}
          .entry p{margin:.4rem 0 0;font-size:.9rem;color:var(--text);display:-webkit-box;-webkit-line-clamp:4;-webkit-box-orient:vertical;overflow:hidden}
          .foot{margin-top:1.4rem;font-size:.82rem;color:var(--muted)}
        </style>
      </head>
      <body>
        <div class="wrap">
          <h1><xsl:value-of select="atom:feed/atom:title"/></h1>
          <p class="sub"><xsl:value-of select="atom:feed/atom:subtitle"/></p>
          <div class="note">This is an Atom feed. Subscribe in any feed reader with this address: <code><xsl:value-of select="atom:feed/atom:link[@rel='self']/@href"/></code></div>
          <xsl:for-each select="atom:feed/atom:entry">
            <div class="entry">
              <h2><a href="{atom:link/@href}"><xsl:value-of select="atom:title"/></a></h2>
              <div class="date"><xsl:value-of select="substring(atom:updated,1,10)"/></div>
              <p><xsl:value-of select="atom:content"/></p>
            </div>
          </xsl:for-each>
          <p class="foot">Site: <a href="{atom:feed/atom:link[not(@rel)]/@href}"><xsl:value-of select="atom:feed/atom:link[not(@rel)]/@href"/></a> · Author: <xsl:value-of select="atom:feed/atom:author/atom:name"/></p>
        </div>
      </body>
    </html>
  </xsl:template>
</xsl:stylesheet>
