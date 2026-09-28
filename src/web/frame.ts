/**
 * The chat iframe's bundle: the host stand-in first, then the chat UI VS Code uses, then the pieces
 * a phone needs around it (a back button, the app's safe-area insets).
 */

import './frame.css';
import { toParent } from './frameHost.ts';
import '../webview/main.ts';

const hdr = document.querySelector('.hdr');
if (hdr) {
  const back = document.createElement('button');
  back.className = 'icon back';
  back.id = 'btnBack';
  back.setAttribute('aria-label', 'Back');
  back.innerHTML = '<i class="codicon codicon-chevron-left"></i>';
  back.onclick = () => toParent({ type: 'nav', to: 'back' });
  hdr.prepend(back);
}
