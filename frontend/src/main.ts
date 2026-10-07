import { createApp } from "vue";
import App from "./App.vue";
import { router } from "./router";
import { installUnauthorizedHandler } from "./stores/session";
import { describeError, logError } from "./lib/async";
import { useToasts } from "./stores/toast";
// 图标字体要排在应用样式之前：字形类没有特殊性可言，先加载可让后写的规则
// 在需要时能盖住它。
import "./styles/remixicon.css";
import "./styles/app.css";

// 401 的全局处理必须在挂载前装好：身份失效的处理逻辑只此一处，
// 页面组件不需要各自判断"这个错误是不是会话过期"。
installUnauthorizedHandler();

const app = createApp(App);

// 渲染期异常不会让整个应用崩掉，但会让"出错的那一次更新"整帧丢掉：界面停在
// 上一帧（例如永远显示"正在加载"），除了控制台没有任何提示。这里至少把它变成
// 一条看得见的提示；同一个错误只提示一次，避免每次重渲染都弹。
let reported = "";
app.config.errorHandler = (err, _instance, info) => {
  logError(`vue.${info}`, err);
  const message = describeError(err);
  if (message === reported) {
    return;
  }
  reported = message;
  useToasts().error("页面渲染出错", message);
};

app.use(router).mount("#app");
