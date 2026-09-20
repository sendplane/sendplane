import { createApp } from 'vue'

import '@sendplane/ui/style.css'

import App from './App.vue'
import { applyTheme } from './auth.js'
import { router } from './router.js'
import './styles.css'

applyTheme()
createApp(App).use(router).mount('#app')
