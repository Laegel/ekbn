import { initApp } from './app.js';
import './styles/style.css';

const custom = document.createElement('link');
custom.rel = 'stylesheet';
custom.href = '/custom.css';
document.head.appendChild(custom);

initApp();
