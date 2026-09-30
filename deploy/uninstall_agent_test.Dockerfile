FROM python:3.12-slim-bookworm
ENV MSBOOST_UNINSTALL_FIXTURE=1
COPY deploy/uninstall-agent.sh deploy/uninstall_agent_test.sh /src/deploy/
COPY deploy/uninstall_agent_integration_test.py deploy/cleanup_agent_integration_test.py deploy/cleanup-agent.sh /src/deploy/
CMD ["bash", "-c", "bash /src/deploy/uninstall_agent_test.sh && python3 /src/deploy/cleanup_agent_integration_test.py"]
