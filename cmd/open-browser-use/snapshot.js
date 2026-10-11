// Compact page snapshot + ref-based actions for LLM agents.
// Evaluated in the page via CDP Runtime.evaluate as `(<this function>)(op, opts)`.
// Returns a short text outline (snapshot) or a small action descriptor (act).
// Only reads the DOM and tags elements with data-obu-ref; it never sends data anywhere.
(function obuSnapshot(op, opts) {
  'use strict';
  opts = opts || {};
  const REF = 'data-obu-ref';
  const win = window;
  win.__obuRefSeq = win.__obuRefSeq || 0;

  const INTERACTIVE = [
    'a[href]', 'button', 'input:not([type=hidden])', 'select', 'textarea', 'summary',
    '[role=button]', '[role=link]', '[role=checkbox]', '[role=radio]', '[role=switch]',
    '[role=tab]', '[role=menuitem]', '[role=option]', '[role=combobox]', '[role=textbox]',
    '[role=searchbox]', '[contenteditable=""]', '[contenteditable=true]',
    '[tabindex]:not([tabindex="-1"])',
  ].join(',');
  const TEXT = 'h1,h2,h3,h4,h5,h6,[role=heading],p,li,tr,dt,dd,legend,caption,figcaption,blockquote,pre';

  const clean = (value, max) => {
    const text = String(value || '').replace(/\s+/g, ' ').trim();
    return text.length > max ? text.slice(0, max - 1) + '…' : text;
  };
  const isShown = (el) => {
    const rect = el.getBoundingClientRect();
    if (rect.width <= 0 || rect.height <= 0) return false;
    const style = getComputedStyle(el);
    return style.visibility !== 'hidden' && style.display !== 'none';
  };
  const inViewport = (el) => {
    const rect = el.getBoundingClientRect();
    return rect.bottom > 0 && rect.top < innerHeight && rect.right > 0 && rect.left < innerWidth;
  };
  const refOf = (el) => {
    let ref = el.getAttribute(REF);
    if (!ref) {
      ref = String(++win.__obuRefSeq);
      el.setAttribute(REF, ref);
    }
    return ref;
  };

  const roleOf = (el) => {
    const explicit = el.getAttribute('role');
    if (explicit) return explicit;
    const tag = el.tagName.toLowerCase();
    if (tag === 'a') return 'link';
    if (tag === 'select') return el.multiple ? 'listbox' : 'combobox';
    if (tag === 'textarea') return 'textbox';
    if (tag === 'summary') return 'button';
    if (tag === 'input') {
      const type = (el.type || 'text').toLowerCase();
      if (['button', 'submit', 'reset', 'image'].includes(type)) return 'button';
      if (['checkbox', 'radio', 'range'].includes(type)) return type;
      if (type === 'search') return 'searchbox';
      return 'textbox';
    }
    if (el.isContentEditable) return 'textbox';
    if (tag === 'button') return 'button';
    return 'clickable';
  };

  const nameOf = (el) => {
    const aria = el.getAttribute('aria-label');
    if (aria) return clean(aria, 80);
    const labelledBy = el.getAttribute('aria-labelledby');
    if (labelledBy) {
      const text = labelledBy.split(/\s+/).map((id) => document.getElementById(id)?.innerText || '').join(' ');
      if (text.trim()) return clean(text, 80);
    }
    if (el.labels && el.labels.length) {
      // Label text minus the control's own value (labels often wrap the input).
      const text = Array.from(el.labels).map((label) => label.innerText).join(' ');
      if (text.trim()) return clean(text, 80);
    }
    const attr = el.getAttribute('placeholder') || el.getAttribute('title') || el.getAttribute('alt');
    if (attr) return clean(attr, 80);
    const tag = el.tagName.toLowerCase();
    if (tag === 'input' && ['button', 'submit', 'reset'].includes((el.type || '').toLowerCase())) {
      return clean(el.value, 80);
    }
    if (tag === 'select' || tag === 'textarea' || tag === 'input') return el.name ? clean(el.name, 40) : '';
    return clean(el.innerText || el.textContent, 80);
  };

  const describe = (el) => {
    const role = roleOf(el);
    const parts = [`[${refOf(el)}] ${role}`];
    const name = nameOf(el);
    if (name) parts.push(JSON.stringify(name));
    const tag = el.tagName.toLowerCase();
    if (tag === 'select') {
      const selected = el.selectedOptions[0];
      parts.push(`value=${JSON.stringify(selected ? clean(selected.text, 40) : '')}`);
      const options = Array.from(el.options).map((o) => clean(o.text, 30)).filter(Boolean);
      parts.push(`options=[${options.slice(0, 12).join('|')}${options.length > 12 ? `|…+${options.length - 12}` : ''}]`);
    } else if (role === 'checkbox' || role === 'radio' || role === 'switch') {
      const checked = 'checked' in el ? el.checked : el.getAttribute('aria-checked') === 'true';
      parts.push(checked ? 'checked' : 'unchecked');
    } else if (role === 'textbox' || role === 'searchbox' || role === 'combobox') {
      const value = el.isContentEditable ? el.innerText : el.value;
      const shown = (el.type || '').toLowerCase() === 'password' && value ? '••••' : clean(value, 60);
      parts.push(`value=${JSON.stringify(shown)}`);
    }
    if (el.disabled || el.getAttribute('aria-disabled') === 'true') parts.push('disabled');
    return parts.join(' ');
  };

  const textLine = (el) => {
    const tag = el.tagName.toLowerCase();
    if (tag === 'tr') {
      const cells = Array.from(el.cells).map((cell) => clean(cell.innerText, 40));
      return cells.some(Boolean) ? `row: ${cells.join(' | ')}` : '';
    }
    const text = clean(el.innerText, opts.text_limit || 200);
    if (!text) return '';
    if (/^h[1-6]$/.test(tag)) return `${tag}: ${text}`;
    if (el.getAttribute('role') === 'heading') return `heading: ${text}`;
    return `${tag}: ${text}`;
  };

  const header = () => {
    const maxScroll = Math.max(0, document.documentElement.scrollHeight - innerHeight);
    return `${clean(document.title, 80)} — ${location.href}\nscroll ${Math.round(scrollY)}/${Math.round(maxScroll)}px`;
  };

  if (op === 'snapshot') {
    const query = String(opts.query || '').toLowerCase();
    const fullPage = Boolean(opts.full_page) || Boolean(query);
    const maxItems = opts.max_items || 150;
    const lines = [];
    let below = 0;
    let above = 0;
    const nodes = document.body ? document.body.querySelectorAll(`${INTERACTIVE},${TEXT}`) : [];
    for (const el of nodes) {
      if (!isShown(el)) continue;
      const interactive = el.matches(INTERACTIVE);
      if (!interactive) {
        // Skip text blocks nested in another text block or that only wrap a control.
        if (el.parentElement && el.parentElement.closest(TEXT)) continue;
        if (el.querySelector(INTERACTIVE) && clean(el.innerText, 1000).length < 60) continue;
      }
      if (!fullPage && !inViewport(el)) {
        if (el.getBoundingClientRect().top >= innerHeight) below++;
        else above++;
        continue;
      }
      const line = interactive ? describe(el) : textLine(el);
      if (!line) continue;
      if (query && !line.toLowerCase().includes(query)) continue;
      if (lines.length < maxItems) lines.push(line);
      else below++;
    }
    const out = [header()];
    out.push(...(lines.length ? lines : [query ? `(no match for ${JSON.stringify(query)})` : '(no visible content)']));
    if (above || below) {
      out.push(`… ${above} item(s) above, ${below} below the viewport — scroll, or snapshot with query to search the whole page`);
    }
    return out.join('\n');
  }

  if (op === 'act') {
    const el = document.querySelector(`[${REF}="${String(opts.ref).replace(/"/g, '')}"]`);
    if (!el) throw new Error(`ref ${opts.ref} not found on this page; take a new snapshot`);
    const action = String(opts.action || 'click').toLowerCase();
    el.scrollIntoView({ block: 'center', inline: 'center' });
    const center = () => {
      const rect = el.getBoundingClientRect();
      return { x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 };
    };
    const setNativeValue = (value) => {
      const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
      const setter = Object.getOwnPropertyDescriptor(proto, 'value')?.set;
      if (setter) setter.call(el, value); else el.value = value;
      el.dispatchEvent(new Event('input', { bubbles: true }));
      el.dispatchEvent(new Event('change', { bubbles: true }));
    };
    if (action === 'click') return { mouse: center() };
    if (action === 'check' || action === 'uncheck') {
      const checked = 'checked' in el ? el.checked : el.getAttribute('aria-checked') === 'true';
      if (checked === (action === 'check')) return { done: `already ${action}ed` };
      return { mouse: center() };
    }
    if (action === 'fill') {
      el.focus();
      if (el.isContentEditable) {
        el.textContent = String(opts.value ?? '');
        el.dispatchEvent(new InputEvent('input', { bubbles: true }));
      } else {
        setNativeValue(String(opts.value ?? ''));
      }
      return { done: 'filled' };
    }
    if (action === 'select') {
      if (el.tagName.toLowerCase() !== 'select') throw new Error(`ref ${opts.ref} is not a <select>`);
      const wanted = String(opts.value ?? '').trim().toLowerCase();
      const option = Array.from(el.options).find((o) => o.value.toLowerCase() === wanted || o.text.trim().toLowerCase() === wanted);
      if (!option) throw new Error(`no option ${JSON.stringify(opts.value)}; options: ${Array.from(el.options).map((o) => o.text.trim()).join(' | ')}`);
      el.value = option.value;
      el.dispatchEvent(new Event('input', { bubbles: true }));
      el.dispatchEvent(new Event('change', { bubbles: true }));
      return { done: `selected ${JSON.stringify(option.text.trim())}` };
    }
    if (action === 'focus' || action === 'press') {
      el.focus();
      return action === 'press' ? { key: String(opts.value || 'Enter') } : { done: 'focused' };
    }
    throw new Error(`unsupported action ${JSON.stringify(action)}; use click|fill|select|check|uncheck|press|focus`);
  }

  if (op === 'scroll') {
    const amount = Math.round(innerHeight * 0.8) * (String(opts.direction) === 'up' ? -1 : 1);
    scrollBy(0, amount);
    return 'scrolled';
  }

  throw new Error(`unknown op ${op}`);
})
