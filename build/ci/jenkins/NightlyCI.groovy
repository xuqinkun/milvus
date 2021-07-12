#!/usr/bin/env groovy

// When scheduling a job that gets automatically triggered by changes,
// you need to include a [cronjob] tag within the commit message.
String cron_timezone = "TZ=Asia/Shanghai"
String cron_string = BRANCH_NAME == "master" ? "50 22 * * * " : ""

int timeout_minutes = 90
int delay_minutes = 5
int ci_timeout = (timeout_minutes - delay_minutes) * 60

pipeline {
    agent none
    triggers {
        cron """${cron_timezone}
            ${cron_string}"""
    }
    options {
        timestamps()
        timeout(time: timeout_minutes, unit: 'MINUTES')
        buildDiscarder logRotator(artifactDaysToKeepStr: '30')
        // parallelsAlwaysFailFast()
    }
    stages {
        stage ('E2E Test') {
            matrix {
                axes {
                    axis {
                        name 'MILVUS_SERVER_TYPE'
                        values 'standalone', 'distributed'
                    }
                    axis {
                        name 'MILVUS_CLIENT'
                        values 'pymilvus', 'pymilvus-orm'
                    }
                }
                agent {
                    kubernetes {
                        label "milvus-e2e-test-kind-nightly"
                        inheritFrom 'default'
                        defaultContainer 'main'
                        yamlFile "build/ci/jenkins/pod/krte.yaml"
                        customWorkspace '/home/jenkins/agent/workspace'
                    }
                }
                environment {
                    PROJECT_NAME = "milvus"
                    SEMVER = "${BRANCH_NAME.contains('/') ? BRANCH_NAME.substring(BRANCH_NAME.lastIndexOf('/') + 1) : BRANCH_NAME}"
                    IMAGE_REPO = "dockerhub-mirror-sh.zilliz.cc/milvusdb"
                    DOCKER_BUILDKIT = 1
                    ARTIFACTS = "${env.WORKSPACE}/_artifacts"
                    DOCKER_CREDENTIALS_ID = "f0aacc8e-33f2-458a-ba9e-2c44f431b4d2"
                    TARGET_REPO = "milvusdb"
                }
                stages {
                    stage('Test') {
                        steps {
                            container('main') {
                                dir ('tests/scripts') {
                                    script {
                                        def clusterEnabled = "false"
                                        if ("${MILVUS_SERVER_TYPE}" == "distributed") {
                                            clusterEnabled = "true"
                                        }

                                        if ("${MILVUS_CLIENT}" == "pymilvus") {
                                            sh """
                                            MILVUS_CLUSTER_ENABLED=${clusterEnabled} \
                                            timeout -v ${ci_timeout} \
                                            ./e2e-k8s.sh \
                                            --kind-config "${env.WORKSPACE}/build/config/topology/trustworthy-jwt-ci.yaml" \
                                            --node-image registry.zilliz.com/kindest/node:v1.20.2
                                            """
                                        } else if ("${MILVUS_CLIENT}" == "pymilvus-orm") {
                                            sh """
                                            MILVUS_CLUSTER_ENABLED=${clusterEnabled} \
                                            timeout -v ${ci_timeout} \
                                            ./e2e-k8s.sh \
                                            --kind-config "${env.WORKSPACE}/build/config/topology/trustworthy-jwt-ci.yaml" \
                                            --node-image registry.zilliz.com/kindest/node:v1.20.2 \
                                            --test-extra-arg "--tags L0 L1 L2"
                                            """
                                        } else {
                                            error "Error: Unsupported Milvus client: ${MILVUS_CLIENT}"
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
                post {
                    unsuccessful {
                        container('jnlp') {
                            script {
                                emailext subject: '$DEFAULT_SUBJECT',
                                body: '$DEFAULT_CONTENT',
                                recipientProviders: [requestor()],
                                replyTo: '$DEFAULT_REPLYTO',
                                to: 'qa@zilliz.com'
                            }
                        }
                    }
                    always {
                        container('main') {
                            script {
                                dir("${env.ARTIFACTS}") {
                                    sh "find ./kind -path '*/history/*' -type f | xargs tar -zcvf artifacts-${PROJECT_NAME}-${MILVUS_SERVER_TYPE}-${SEMVER}-${env.BUILD_NUMBER}-e2e-nightly-logs.tar.gz --transform='s:^[^/]*/[^/]*/[^/]*/[^/]*/::g' || true"
                                    if ("${MILVUS_CLIENT}" == "pymilvus-orm") {
                                        sh "tar -zcvf artifacts-${PROJECT_NAME}-${MILVUS_SERVER_TYPE}-${MILVUS_CLIENT}-pytest-logs.tar.gz ./tests/pytest_logs --remove-files || true"
                                    }
                                    archiveArtifacts artifacts: "**.tar.gz", allowEmptyArchive: true
                                    sh 'docker rm -f \$(docker network inspect -f \'{{ range \$key, \$value := .Containers }}{{ printf "%s " \$key}}{{ end }}\' kind) || true'
                                    sh 'docker network rm kind > /dev/null 2>&1 || true'
                                }
                            }
                        }
                    }
                    success {
                        container('main') {
                            script {
                                def date = sh(returnStdout: true, script: 'date +%Y%m%d').trim()
                                def gitShortCommit = sh(returnStdout: true, script: 'git rev-parse --short HEAD').trim()

                                withCredentials([usernamePassword(credentialsId: "${env.DOCKER_CREDENTIALS_ID}", usernameVariable: 'DOCKER_USERNAME', passwordVariable: 'DOCKER_PASSWORD')]) {
                                    sh 'docker login -u ${DOCKER_USERNAME} -p ${DOCKER_PASSWORD}'
                                    sh """
                                        docker tag localhost:5000/milvus:latest ${TARGET_REPO}/milvus-nightly:${env.BRANCH_NAME}-${date}-${gitShortCommit}
                                        docker tag localhost:5000/milvus:latest ${TARGET_REPO}/milvus-nightly:${env.BRANCH_NAME}-latest
                                        docker push ${TARGET_REPO}/milvus-nightly:${env.BRANCH_NAME}-${date}-${gitShortCommit}
                                        docker push ${TARGET_REPO}/milvus-nightly:${env.BRANCH_NAME}-latest
                                    """
                                    sh 'docker logout'
                                }
                            }
                        }
                    }
                    cleanup {
                        container('main') {
                            script {
                                sh 'find . -name . -o -prune -exec rm -rf -- {} +' /* clean up our workspace */
                            }
                        }
                    }
                }
            }
        }
    }
}
