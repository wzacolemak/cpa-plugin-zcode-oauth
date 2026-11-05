      (() => {
        const modalID = 'zcode-upstream-modal';
        const linkClass = 'zcode-upstream-open';
        const switchEndpoint = '/v0/resource/plugins/zcode/upstream';

        const style = document.createElement('style');
        style.textContent = `
          a.${linkClass}{color:var(--primary-color,#3b82f6);cursor:pointer;font-size:12.5px;margin-left:8px;text-decoration:none}
          a.${linkClass}:hover{text-decoration:underline}
          #${modalID}{position:fixed;inset:0;z-index:99999;display:flex;align-items:center;justify-content:center}
          #${modalID} .zu-backdrop{position:absolute;inset:0;background:rgba(0,0,0,.62);backdrop-filter:blur(2px)}
          #${modalID} .zu-dialog{position:relative;width:min(560px,92vw);max-height:88vh;overflow:auto;background:var(--bg-secondary,#232529);color:var(--text-primary,#e8e8ea);border:1px solid var(--border-color,#3a3c42);border-radius:14px;padding:18px 20px;font-family:system-ui,'Microsoft YaHei',sans-serif;box-shadow:0 18px 60px rgba(0,0,0,.5)}
          #${modalID} .zu-head{display:flex;justify-content:space-between;align-items:center}
          #${modalID} .zu-title{font-size:16px;font-weight:700}
          #${modalID} .zu-x{cursor:pointer;color:var(--text-secondary,#9a9ba1);font-size:20px;line-height:1;border:0;background:none}
          #${modalID} .zu-sub{color:var(--text-secondary,#9a9ba1);font-size:12.5px;margin:6px 0 12px}
          #${modalID} .zu-opt{display:flex;gap:8px;align-items:flex-start;padding:10px 12px;border:1px solid var(--border-color,#3a3c42);border-radius:9px;margin-bottom:8px;cursor:pointer}
          #${modalID} .zu-opt:hover{border-color:var(--primary-color,#3b82f6)}
          #${modalID} .zu-opt.cur{border-color:var(--primary-color,#3b82f6)}
          #${modalID} .zu-opt input{margin-top:3px;accent-color:var(--primary-color,#3b82f6)}
          #${modalID} .zu-opt .zu-opt-title{font-size:13.5px;font-weight:600}
          #${modalID} .zu-opt .zu-opt-desc{color:var(--text-secondary,#9a9ba1);font-size:11.5px;margin-top:3px;line-height:1.5}
          #${modalID} .zu-opt .zu-planline{color:var(--text-secondary,#9a9ba1);font-size:11.5px;margin-top:3px}
          #${modalID} .zu-opt .zu-planline b{color:var(--text-primary,#e8e8ea);font-weight:600}
          #${modalID} .zu-keyrow{margin:10px 0 4px}
          #${modalID} .zu-keyrow label{display:block;font-size:12.5px;color:var(--text-secondary,#9a9ba1);margin-bottom:5px}
          #${modalID} input[type=text]{width:100%;box-sizing:border-box;background:#1d1f24;color:var(--text-primary,#e8e8ea);border:1px solid var(--border-color,#3a3c42);border-radius:7px;padding:7px 10px;font-size:12.5px}
          #${modalID} .zu-actions{display:flex;justify-content:flex-end;gap:8px;margin-top:14px}
          #${modalID} .zu-btn{background:var(--primary-color,#3b82f6);color:#fff;border:0;border-radius:7px;padding:7px 16px;font-size:13px;cursor:pointer}
          #${modalID} .zu-btn.ghost{background:var(--bg-tertiary,#2b2d33);color:var(--text-primary,#e8e8ea);border:1px solid var(--border-color,#3a3c42)}
          #${modalID} .zu-msg{font-size:12.5px;margin-top:10px;min-height:16px}
          #${modalID} .zu-msg.ok{color:var(--success-color,#34d399)}
          #${modalID} .zu-msg.err{color:var(--error-color,#ef4444)}
          #${modalID} .zu-loading{color:var(--text-secondary,#9a9ba1);font-size:12.5px}
        `;
        document.head.appendChild(style);

        function esc(s) { return String(s ?? '').replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c])); }

        function findFileNameSpan() {
          return [...document.querySelectorAll('span[class*="fileName"]')].find(el =>
            el.textContent.trim() === "zcode.json");
        }

        function ensureLink() {
          const onAuthPage = location.hash.startsWith('#/auth-files');
          const existing = document.querySelector(`a.${linkClass}`);
          if (!onAuthPage) { if (existing) existing.remove(); return; }
          const span = findFileNameSpan();
          if (!span) { if (existing) existing.remove(); return; }
          if (existing && existing.parentElement === span.parentElement) return;
          if (existing) existing.remove();
          const link = document.createElement('a');
          link.className = linkClass;
          link.textContent = '🔀 套餐';
          link.href = 'javascript:void(0)';
          link.addEventListener('click', openModal);
          span.parentElement.insertBefore(link, span.nextSibling);
        }

        async function fetchStatus() {
          const resp = await fetch(switchEndpoint + '?format=json', { cache: 'no-store' });
          return resp.json();
        }

        function planLines(st) {
          const relay = (st.relay_plans || []).map(p => {
            const bits = [p.name ? `<b>${esc(p.name)}</b>` : '', p.status ? esc(p.status) : '', p.window ? esc(p.window) : '', p.effective ? `生效 ${esc(p.effective)}` : '', p.models ? esc(p.models) : ''].filter(Boolean);
            return bits.length ? `<div class="zu-planline">· ${bits.join(' · ')}</div>` : '';
          }).join('');
          const direct = (st.direct_plans || []).map(p => {
            const bits = [p.product ? `<b>${esc(p.product)}</b>` : '', p.status ? esc(p.status) : '', p.valid ? esc(p.valid) : ''].filter(Boolean);
            return bits.length ? `<div class="zu-planline">· ${bits.join(' · ')}</div>` : '';
          }).join('');
          const warn = st.detect_error ? `<div class="zu-planline">套餐检测失败: ${esc(st.detect_error)}</div>` : '';
          return { relay, direct, warn };
        }

        function openModal() {
          let m = document.getElementById(modalID);
          if (m) { m.style.display = 'flex'; return; }
          m = document.createElement('div');
          m.id = modalID;
          m.innerHTML = `
            <div class="zu-backdrop"></div>
            <div class="zu-dialog">
              <div class="zu-head"><span class="zu-title">zcode 套餐切换</span><button class="zu-x" type="button">×</button></div>
              <p class="zu-sub" id="zu-sub">检测套餐中…</p>
              <label class="zu-opt" id="zu-opt-auto"><input type="radio" name="zu-mode" value="auto">
                <span><span class="zu-opt-title">自动 · 优先体验套餐（relay，并发 1）</span>
                <div class="zu-opt-desc">检测到 relay 体验套餐就用它跑它覆盖的模型（默认 glm-5.3 / glm-5.3-flash）；并发占满、套餐不可用或请求失败时自动回落正常渠道（Ultra），不排队不报错</div>
                <div id="zu-auto-plans"></div></span></label>
              <label class="zu-opt" id="zu-opt-relay"><input type="radio" name="zu-mode" value="relay">
                <span><span class="zu-opt-title">start plan（免费 · 体验套餐）· zcode 中转</span>
                <div class="zu-opt-desc">OAuth JWT → zcode.z.ai 中转，套餐由服务端路由（活动加速卡期间部分模型可能被遮蔽）</div>
                <div id="zu-relay-plans"></div></span></label>
              <label class="zu-opt" id="zu-opt-ultra"><input type="radio" name="zu-mode" value="ultra">
                <span><span class="zu-opt-title">Ultra（个人 · 1.5x 额度）</span>
                <div class="zu-opt-desc">长期 CodingPlan key → zcode.z.ai/api/v1/ultra/anthropic；官方 1.5x 配额口径，实测扣减 ≈0.67x 直连</div>
                <div id="zu-ultra-plans"></div></span></label>
              <div class="zu-keyrow" id="zu-keyrow">
                <label>CodingPlan API key（留空保留现有）</label>
                <input type="text" id="zu-key" placeholder="xxxxxx.yyyyyyyy" autocomplete="off">
              </div>
              <div class="zu-msg" id="zu-msg"></div>
              <div class="zu-actions"><button class="zu-btn ghost" type="button" id="zu-cancel">取消</button><button class="zu-btn" type="button" id="zu-save">保存切换</button></div>
            </div>`;
          document.body.appendChild(m);
          m.querySelector('.zu-backdrop').addEventListener('click', closeModal);
          m.querySelector('.zu-x').addEventListener('click', closeModal);
          m.querySelector('#zu-cancel').addEventListener('click', closeModal);
          m.querySelectorAll('input[name=zu-mode]').forEach(r => r.addEventListener('change', () => {
            m.querySelector('#zu-keyrow').style.display = m.querySelector('input[name=zu-mode]:checked').value === 'relay' ? 'none' : '';
          }));
          m.querySelector('#zu-save').addEventListener('click', saveSwitch);
          refreshModal();
        }

        async function refreshModal() {
          const m = document.getElementById(modalID);
          if (!m) return;
          const sub = m.querySelector('#zu-sub');
          sub.textContent = '检测套餐中…';
          let st;
          try {
            st = await fetchStatus();
          } catch (err) {
            sub.textContent = '套餐检测失败: ' + String(err);
            return;
          }
          sub.innerHTML = `当前: <b>${esc(st.label || st.mode)}</b>${st.direct_key_set ? ' · Bigmodel key 已配置' : ' · Bigmodel key 未配置'}`;
          const lines = planLines(st);
          m.querySelector('#zu-relay-plans').innerHTML = lines.relay || '<div class="zu-planline">· 未检测到中转侧套餐</div>';
          m.querySelector('#zu-ultra-plans').innerHTML = (st.direct_plans || []).map(p => {
            const bits = [p.product ? `<b>${esc(p.product)}</b>` : '', p.status ? esc(p.status) : '', p.valid ? esc(p.valid) : ''].filter(Boolean);
            return bits.length ? `<div class="zu-planline">· ${bits.join(' · ')}</div>` : '';
          }).join('') || '<div class="zu-planline">· 未检测到 CodingPlan 订阅</div>';
          m.querySelector('#zu-opt-relay').classList.toggle('cur', st.mode === 'relay');
          m.querySelector('#zu-opt-ultra').classList.toggle('cur', st.mode === 'ultra');
          m.querySelector('#zu-opt-auto').classList.toggle('cur', st.mode === 'auto');
          const trial = st.relay_trial;
          const capacity = `${st.relay_inflight || 0}/${st.relay_max_concurrency || 1}`;
          const fallback = st.fallback_label || st.fallback_mode || 'Ultra';
          let autoLines = '';
          if (trial) {
            autoLines += trial.available
              ? `<div class="zu-planline">✓ 体验套餐可用${trial.plans && trial.plans.length ? ' · ' + trial.plans.map(esc).join(' · ') : ''}</div>`
              : `<div class="zu-planline">✕ 体验套餐不可用${trial.reason ? ' · ' + esc(trial.reason) : ''}</div>`;
            if (trial.available && trial.models && trial.models.length) {
              autoLines += `<div class="zu-planline">覆盖模型: ${trial.models.map(esc).join(', ')}${trial.default_models ? '（默认口径）' : ''}</div>`;
            }
          }
          autoLines += `<div class="zu-planline">并发占用 ${esc(capacity)} · 超出回落 ${esc(fallback)}</div>`;
          m.querySelector('#zu-auto-plans').innerHTML = autoLines;
          m.querySelectorAll('input[name=zu-mode]').forEach(r => { r.checked = r.value === st.mode; });
          m.querySelector('#zu-keyrow').style.display = st.mode === 'relay' ? 'none' : '';
        }

        function closeModal() {
          const m = document.getElementById(modalID);
          if (m) m.remove();
        }

        document.addEventListener('keydown', e => { if (e.key === 'Escape') closeModal(); });

        async function saveSwitch() {
          const m = document.getElementById(modalID);
          const msg = m.querySelector('#zu-msg');
          const mode = m.querySelector('input[name=zu-mode]:checked').value;
          const key = m.querySelector('#zu-key').value.trim();
          msg.className = 'zu-msg';
          msg.textContent = '保存中…';
          try {
            const u = `${switchEndpoint}?format=json&mode=${encodeURIComponent(mode)}${key ? '&key=' + encodeURIComponent(key) : ''}`;
            const resp = await fetch(u, { cache: 'no-store' });
            const d = await resp.json();
            if (d.ok) {
              msg.className = 'zu-msg ok';
              msg.textContent = '✓ ' + (d.message || '已切换');
              setTimeout(() => { closeModal(); }, 900);
            } else {
              msg.className = 'zu-msg err';
              msg.textContent = '✕ ' + (d.message || '切换失败');
            }
          } catch (err) {
            msg.className = 'zu-msg err';
            msg.textContent = '✕ ' + String(err);
          }
        }

        // 调度: 不用 requestAnimationFrame (隐藏/被遮挡的标签页里 rAF 永不触发, 会导致按钮永远不出现);
        // setTimeout 防抖 + hashchange + 2s 看门狗三重兜底
        let linkTimer = null;
        function scheduleLink() {
          if (linkTimer) return;
          linkTimer = setTimeout(() => { linkTimer = null; ensureLink(); }, 200);
        }
        window.addEventListener('hashchange', scheduleLink);
        setInterval(ensureLink, 2000);

        const obs = new MutationObserver(mutations => {
          for (const mut of mutations) {
            const t = mut.target;
            if (t && (t.id === modalID || (t.closest && t.closest(`#${modalID}`)))) continue;
            scheduleLink();
            return;
          }
        });
        obs.observe(document.body, { childList: true, subtree: true });
        ensureLink();
      })();
