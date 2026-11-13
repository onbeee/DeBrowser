import React from "react";
import ReactDOM from "react-dom/client";
import WelcomeApp from "./welcome/WelcomeApp";
import "./index.css";

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <WelcomeApp />
  </React.StrictMode>,
);
