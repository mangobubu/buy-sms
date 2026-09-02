import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import 'element-plus/dist/index.css'
import '@/styles/index.css'
import App from './App.vue'
import router from './router'
import { authSession } from '@/stores/auth'

const app = createApp(App)

authSession.bindUnauthorizedHandler(router)

app.use(router)
app.use(ElementPlus, { locale: zhCn })
app.mount('#app')
